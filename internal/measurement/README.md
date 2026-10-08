# Native usage persistence and measurement

Independent package for Pacer `PLAN.md §3.1` and `CACHE-TTL.md §§2,6`.
It uses the caller's SQLite `*sql.DB`; no CPA ABI, provider calls, CPAMP queue,
router mutation, or historical migration is inside this package.

## Integration API

```go
store, err := measurement.Open(ctx, db, measurement.Options{Clock: clock})
err = store.RegisterPriceBook(ctx, measurement.DefaultPriceBook())
err = store.StartDelivery(ctx, uniqueObserverIncarnationID, startupTime)

result, err := store.PutRecord(ctx, record)
err = store.PutObservation(ctx, observation)
err = store.CloseCycle(ctx, sourceBackedClosure)
err = store.Checkpoint(ctx, incarnationID, drainedThrough, false)
err = store.PutGap(ctx, measurement.Gap{
    ID: stableGapID, From: firstPossiblyMissing, To: lastPossiblyMissing,
    Reason: "callback_overflow",
})

report, err := store.Measure(ctx, measurement.Query{
    Provider: "claude", AccountID: accountID, Scope: "all",
    BookID: measurement.DefaultPriceBook().ID,
    Since: now.Add(-21 * 24 * time.Hour), AsOf: now,
})
expected := report.Expected(chatRef, measurement.TTL1h)
runway := report.WarmBurn.Runway(availableUSD, now, verifiedReset)
```

Check every error. `Open` creates only an empty schema once, validates version 1
on reopen, and does not acknowledge delivery. It does not close/reconfigure the
database. Use the owner's durable SQLite configuration (e.g. WAL, FULL synchronous,
busy timeout); the package works with `SetMaxOpenConns(1)` shared with other stores.
All transaction queries use that transaction; rows are closed before subsequent
queries. Writes lock the metadata row before any read/check/write operation.

`Measure`, `Record`, `ValueRecord`, `Expected`, and `Runway` have no write effects.
`Measure` takes a consistent read transaction, releases it, then analyzes in memory.
Run it on an analytical/background path and publish/cache the report for routing;
do not scan usage history inside `scheduler.pick`. It reads the requested provider's
population for TTL priors, but quota ratios and burn are for the requested account.
The default read limit is 100,000 rows per evidence kind; exceeding it returns
`ErrLimit`, not a silently truncated estimate. Scope/model membership is supplied by
the observer, not inferred from model name substrings. `WarmBurn` is account-wide.

Router mapping:

- `FiveHour.Known` → `FiveHour.USDPerPercent`; otherwise use unmeasured headroom.
- `SevenDay.Known` → `SevenDay.USDPerPercent`; otherwise equal account sizes.
- `Expected.Known` → `Expected.USD`; otherwise no manufactured stretch prior.
- `WarmBurn.Bounded` → use its **upper** rate for the runway guard. A false flag
  is unknown load, not zero. `Runway` blocks new placement on unknown burn/budget;
  it never asks to move an existing chat. Check `Known`/`Reason`; `Hours=0` for
  `no_observed_warm_burn` is not a zero runway.
- Tracker/policy still owns current affinity, in-flight admission and managed TTL
  validation. Do not replace unknown legacy affinity using this bounded history.

## Record contract and revision semantics

The adapter supplies an explicit independent `Record`:

- Globally stable `EventID` per **physical attempt**, `SourceID`, positive
  cumulative `Revision`. Retries use distinct event/attempt IDs, even with the
  same logical `RequestID`. `(SourceID, AttemptID)` is additionally unique when
  an attempt ID is supplied. An attempt relabelled with another event ID conflicts.
- Immutable request/attempt/stream/trace/execution IDs; caller, installation,
  session, account, credential/key **references**, resolved/requested model,
  service tier, client/node/fork/compaction attribution. No raw key field exists.
- Actual upstream `StartedAt`; `EndedAt` when known. `Final` distinguishes settled
  usage from an in-flight/cumulative snapshot. Paid failed attempts are retained.
- Immutable policy epoch/revision, selected/effective TTL, per-attempt start time
  and optional executor `AllWritesCovered` evidence. Never look up the chat's
  current policy when delayed usage arrives. The optional policy start, if present,
  must equal the record's upstream start.
- Explicit disjoint normalized `Usage`: noncache input, read, aggregate creation,
  optional observed 5m/1h writes, output including reasoning once, and optional
  conservation totals. `Quality=complete` requires a nonempty accounting version.
  The adapter must convert CPA's actual accounting convention; guessing legacy
  input semantics is outside this package. Invalid counts remain stored evidence,
  but invalidate valuation. Missing split pointers stay missing, not zero.
- `RawUsage` is bounded JSON containing source **usage**, not a full request or
  authorization headers. Exact bytes are stored separately and retained on every
  revision; normal JSON marshaling excludes it. `CapturedCostNanoUSD` is retained
  as a separate fact, not substituted for the pinned price-book value.

Activity evidence is also immutable: capture the primary-prefix relationship and
effective protection policy from the attempt's dispatched payload. Successful,
final usage with positive confirmed read/create counts establishes a refresh;
the mere intended relationship does not. If the host only emits final native usage,
use revision 1. If it emits cumulative snapshots, increase the revision and resend
the **entire** snapshot. Higher revisions replace current usage without adding it;
all old revisions remain in `measurement_record_revisions`. Identical deliveries
of any retained revision are idempotent. Conflicting equal/older revisions, changed
identity/policy/activity, or regression of a settled outcome return `ErrConflict`.
Settled end time and success are immutable; later split enrichment is permitted.

`Record(ctx,eventID,revision)` reads an exact revision; `revision=0` reads current.
This is local evidence, not a public resource API. Reports contain private
attribution across callers and require management authorization or adapter-side
ownership filtering. Session IDs are not authorization.

## Native delivery and restart gaps

Native CPA callbacks are in-memory, not a replay log. Every report explicitly says
`native-in-memory-best-effort`. `Coverage.Observed` means the requested interval is
inside acknowledged native delivery coverage with no **known** gaps; it does not
assert complete provider history or absence of unobservable simultaneous outside use.

Use a fresh incarnation ID on startup/reload. `StartDelivery` closes the preceding
run at its last acknowledged horizon and records a durable restart/reload gap.
There is no invented pre-first-start history. `Checkpoint` is an explicit drained
delivery horizon, not an automatic timer/heartbeat or management-read side effect.
A supplied horizon cannot be in the clock's future. It does not erase marked gaps.
On clean shutdown, drain and checkpoint with `close=true`.

The host must supply a conservative safe horizon and report callback overflow,
failed persistence, dropped records, disconnected hooks and reconciliation gaps.
If it cannot establish such a horizon, leave coverage unknown: ratios will remain
unmeasured. A callback that fails while the database is unavailable needs an
adapter-side pending gap to persist after recovery. This package does not pretend
that synchronous SQLite persistence makes the upstream callback transport durable.
It neither subscribes to nor consumes CPAMP's destructive queue.

## Valuation and price provenance

Valuation version: `pacer-native-value-v1`. Built-in immutable book:
`pacer-prices-2026-10-08-opus55-v1` / `2026-10-08.1`.

The user-pinned Opus 5.5 standard rates per million are **$4 input, $.20 read,
$5 5m write, $8 1h write, $20 output**. The complete observed split of 250k 5m
+ 750k 1h writes is exactly **$7.25**. Only an unclassified remainder introduces
the write-price range. Selected TTL alone never resolves it. Inference requires
`AllWritesCovered` and a compatible effective policy, and remains labelled
`policy_inferred_writes`. Contradictory observed categories suppress inference;
observed splits remain primary cost evidence. Zero writes do not create TTL
uncertainty. Money uses checked, rounded integer nano-USD category totals.

Additional books require explicit model/provider/service-tier/context entries and
source references. Nil category prices are unknown; explicitly configured zero is
free. Context ranges are `[MinInput,MaxInput)` on normalized total input, and
overlap is rejected. Actual `default`/empty/`standard` tiers share standard pricing;
`auto`, priority, fast, flex, batch and unknown tiers require explicit entries.
There is no silent alias fallback, provider-wide multiplier, or current-model guess.

Offline local audit (no remote price lookup or production database):

- CPAMP checkout `cpamp-model-settings/src`, commit
  `6500f6221ca80f6b34b932c4b756d5ab2a3193da`:
  `apps/manager-server/internal/service/usagehistory/api_value.go` pins versioned
  comparison books from stored prices, retains raw normalized/split evidence, and
  treats unresolved writes separately. Its reference unit lists GPT-5.6 Sol input
  at $4/M, while `service/pricing/cost.go:officialGPT56Price` falls back to $5/M
  ($30 output, $.50 read); Terra/Luna fallback tables and legacy tier/context
  multipliers also exist. This discrepancy is not silently resolved here.
- `api_value_test.go:valueTestBook` includes historical model/Opus/Haiku/Gemini
  fixtures; test fixture rates do not establish current production model prices.
- CPA local `pacer/worktrees/cpa/internal/registry/models/models.json` was inspected
  for current Claude, GPT-5.6 and Gemini IDs. The catalogue identifies models and
  capabilities, not authoritative billable rates. The worktree had advanced to
  `d314c85d7161f6147359c78d975d12ec372abfad` when its commit was recorded.

Consequently the default book deliberately leaves other models/tiers unpriced.
Their records survive ingestion, but prevent false exact ratios/burn. A verified
additional price book can revalue retained facts without mutating them. Historical
whole-window assumed-5m valuation/import belongs to the analytics workstream; this
native package never applies that historical assumption to future unknown writes.

## Measurements and warmth

Observations retain source, timestamp, exact scope/model membership and cycle ID.
Only explicit source-backed `CloseCycle` establishes completion; a scheduled reset
passing does not. Adjacent observations are matched to contained attempt intervals.
Crossing/in-flight requests retain separate allocation bounds and disqualify an
exact ratio even when TTL pricing is exact. Any meter increase without an observed
CPA request excludes the window. Meter corrections/conflicts, unusable observations,
delivery gaps, missing prices, partial accounting, and unknown write costs also
exclude it. At least **10 percentage points** of observed movement are required.

- 5h: latest two qualifying **completed** cycles. A single completed cycle may
  seed the ratio. If their ratios differ by more than 1.5×, choose the lower;
  otherwise divide their combined value by their combined delta (delta weighting).
- 7d: current/latest cycle once qualifying at ≥10 points; otherwise latest
  qualifying completed cycle. No synthetic initial zero/terminal 100% observation.

Known quota boundaries are still only observations: API-equivalent value is not
the provider's bill or proof that subscription quota uses these token weights.
Do not sum overlapping 5h/7d window values.

Warm stretches are ordered by upstream request start and scoped to
provider/caller/installation/session/route/account/context epoch and compatible
effective protection TTL. Account changes, epoch changes, and TTL-expiry gaps split
stretches, including A→B→A. Delayed old usage retains its old scope. Successful
primary and same-prefix/account/epoch warming can refresh; failure/title/summary
and mismatched supplied prefix references do not. Paid primary failures inside a
stretch contribute burn without extending protection. `Activity.ProtectionTTL`
and effective TTL protect the longest known relevant prefix; selected TTL is ignored.
Partial/unknown prefix evidence remains conservative unknown warmth. A long stream
holds in-flight protection but does not add a fresh TTL on completion.

Population priors use completed, exact, covered stretches starting in the last
seven days with ≥3 qualifying requests, separately by provider/TTL. Unknown and
left-censored stretches cannot become priors. p75 is nearest-rank. A chat's expected
next stretch is `max(last completed stretch for this account/epoch, p75)`;
population count and sparse-data confidence accompany it. The last stretch may
be older than seven days if included in the explicit query history; the population
never is. An unknown last stretch makes the result unknown rather than silently
falling back to cheaper older history. No 5m prior is reused for 1h. Current burn
divides observed stretch value
by elapsed hours since its upstream start. Unknown burn is not replaced by zero;
missing delivery over the preceding maximum supported 1h horizon also makes it
unknown, because an entire warm chat could be missing.

`EvidenceID` binds the book, query and current immutable evidence revisions. It is
a fingerprint, not a stored report/replay checkpoint. `AsOf` is an event-time cut;
late usage enrichment changes the fingerprint and can make an earlier interval
measurable. Store a report explicitly if historical knowledge-time replay is needed.

## Verification / remaining boundary

Tests use real temporary WAL SQLite files, one-connection/shared-pool reads/writes,
deterministic clocks, restart/revision/dedup, mixed/unknown writes, absent prices,
outside use, allocation boundaries, scoped pools, TTL warmth, qualifying warming,
population segmentation and runway. They use synthetic fixtures, not claimed live
provider evidence. Run `go test -race -count=1 ./internal/measurement` with the
subject-local caches. Actual command logs are under
`pacer/evidence/implementation/measurement-*.log`.

Verified 2026-10-08: `go test -race -count=1 -timeout=120s -v
./internal/measurement` passed; `go vet ./internal/measurement` passed. Receipts:
`measurement-race.log` and `measurement-vet.log` (empty successful vet output).

The parent integration still supplies the additive CPA adapter, normalized fields,
physical-attempt identity, immutable policy evidence, observer samples/closures,
drained delivery horizons and dropped-event diagnostics. No native-host end-to-end
acceptance, CPAMP historical migration/parity, production activation, remote push,
or live provider test is claimed by these package tests.
