# Observation integration contract

`internal/observation` is the durable, header-first quota store and shared direct-request
scheduler. It has no HTTP client, host SDK dependency, worker goroutine, routing decision,
credential discovery, or UI dependency. The bridge supplies provider evidence and executes
only acquired jobs. Tables are prefixed `observation_` and can share the plugin's SQLite DB.

## API

```go
func Open(db *sql.DB, clock func() time.Time) (*Store, error) // nil clock = time.Now
func Headers(provider string, h http.Header, at time.Time) []Window
func API(provider string, body []byte, at time.Time) ([]Window, error)
func Endpoint(provider string) string // claude:usage / codex:usage; unknown = ""
func RetryAfter(h http.Header, now time.Time) (time.Time, bool)

func (*Store) Observe(account, provider string, samples []Window) error
func (*Store) Windows(account string) []Window
func (*Store) ReadWindows(account string) ([]Window, error)
func (*Store) Err() error
func (*Store) History(account string, afterID int64, limit int) ([]HistoryEntry, error)

func (*Store) Queue(account, endpoint, reason string, due time.Time) error
func (*Store) Acquire(account string) (*Job, error)
func (*Store) Start(account string) (*Job, error) // alias for Acquire
func (*Store) Finish(job Job, status int, headers http.Header) error
func (*Store) Status(account string) (Job, bool, error)
func (*Store) Jobs() ([]Job, error)
func (*Store) Attempts(account string, afterID int64, limit int) ([]Attempt, error)
func (*Store) RecoverInterrupted(job Job) error
func (*Store) Resume(account string) error
```

The caller opens/closes the DB and registers its SQLite driver. Production code should use
`ReadWindows` so DB failures remain explicit. `Windows` is a convenience snapshot; it returns
nil on read failure and retains the last failure in `Err`. Returned windows are independent
copies. Multiple stores, DB pools and cooperating processes coordinate through SQLite
`BEGIN IMMEDIATE` transactions and an additional unique endpoint-inflight index. Each write
connection sets a 5s SQLite busy timeout; errors propagate rather than authorizing a request.

### Identity and endpoint budgets

Use a provider-qualified **verified account UUID/ID** for `account`. Credential aliases for that
verified account must use the same string. Until verified, use a provider-qualified credential
ID without merging it with other credentials. There is no email lookup or identity-merging
heuristic in this package.

`endpoint` is a stable **shared budget ID**, never a hostname or request URL. `claude` and
`codex` normalize to `claude:usage` and `codex:usage`. URLs are rejected. Register other stable
budget IDs only for a supported bridge adapter; all its alternative hosts and sibling accounts
must use that same ID. An account cannot change its already registered endpoint budget through
`Queue`. This package cannot police clients that bypass it or invent unrelated account IDs.

## Evidence and time

`Window` exports:

- `Provider`, `Scope`, actual `Seconds`; `Key()` combines these identities.
- `Remaining *float64` in percent: nil means missing, and zero means measured exhaustion.
- `Reset time.Time`, preserving available nanosecond precision and UTC parsing.
- Independent `Usage`, `ResetEvidence`, `StatusEvidence` fields, each with `At`, `Source`,
  `Quality`, `Raw`. Times are verification/source times, not database receipt times.
- Explicit `NotApplicable` and `Dormant`; neither synthesizes a reset.
- Derived `Cycle`, `CycleAt`, `Conflict`, `ConflictAt`, `Ambiguous`, and
  `ConflictingReset` / `ConflictingResetEvidence` for disputed reset evidence.

`Applies(model)` supports shared/OAuth scopes, Claude Opus/Sonnet/Haiku, verified Spark /
Bengalfox aliases, and an adapter-provided `model:<exact-name>` scope. Unknown named limits
are preserved as `limit:<name>` but are not speculatively assigned to an ordinary model.
Code-review, Fable and overage scopes do not gate ordinary inference.

`State(now)` is one of `current`, `estimated`, `partial`, `unknown`, `conflicting`, `ambiguous`,
`awaiting-confirmation`, `dormant`, `not-applicable`. Conflict/ambiguity have priority. Inspect
`Reset` directly as well when displaying an expired conflicting record.
`Complete(now)`, `FreshComplete(now, maxAge)`, `BalanceAge(now)`, `ResetAge(now)` and
`ObservedAt()` expose field-level confidence. Check field presence before displaying ages:
missing/future field times return zero age. State alone is **not** a service-availability gate.

Always pass actual response/event capture time as parser `at`, including stream-final and
synthesized Codex events. Relative resets anchor to that time, remain `estimated`, and never
use delayed callback delivery time. A bridge with a trustworthy provider timestamp can supply
it as `at`; the parser does not replace high-precision capture time with the coarse HTTP Date.
Calling `Observe` later does not change the anchor. Parsed raw timestamp strings are retained,
including precision beyond Go's nanosecond representation.

Usage-only and reset-only samples merge independently in a compatible cycle. Missing fields
never erase existing evidence or refresh its age. A no-signal parser result creates no history
row. A rebound in remaining capacity without reset evidence flags ambiguity. Time reaching a
reset only changes derived state: there are no holds, token-derived observations, fabricated
expiry dates, reset roll-forwards, or automatic replenishment.

Full, strictly newer coherent evidence can replace a cycle **before** its old deadline. A 2s
reset comparison tolerance preserves cycle identity while storing the exact newly reported
reset. Retired cycles stay fenced across restarts, even if a stale snapshot is delivered with
a newer receipt time. Equal-time contradictions and unsupported reset-only cycle changes stay
flagged; full evidence newer than the conflict resolves them. The last coherent reset stays in
`Reset`; display `ConflictingReset` as disputed when present, rather than presenting either
as a verified countdown. `History` preserves accepted, stale and conflicting input samples,
source times, receipt times and dispositions append-only. Receipt time never wins a conflict.
An estimated reset can establish a new cycle over an older absolute reset when complete
evidence also shows replenishment with a later reset, or the preceding cycle has passed.
It remains estimated. A relative-only correction does not downgrade an absolute deadline.

Codex additionally accepts a complete, strictly newer reset correction within **60 seconds
of the current cycle's original reset anchor**, while both deadlines remain in the future
and remaining quota does not increase. It preserves cycle identity and source timestamps,
stores the exact corrected deadline, and never downgrades absolute evidence to estimated
evidence. The fixed anchor prevents successive corrections from walking the tolerance.
This addresses observed weekly reset drift (`09:33:59` → `09:34:11` → `09:33:59`) that
otherwise retires the live allowance and freezes its balance. Other providers retain the
2-second rule. Replenishment, incomplete evidence and genuine retired-cycle replays do
not qualify for this exception.

On upgrade, `codex-reset-drift-v1` examines Codex windows with nearby retirement markers.
It requires the original accepted/conflicting audit records to reconstruct the existing
projection exactly. It removes only markers proved to have come from these corrections,
preserving markers for genuine cycle changes and unexplained markers. Audit history is
unchanged; the next live observation supplies the new balance. The repair and its completion
record are committed in the same startup transaction, and repeated opens are idempotent.

### Verified parser coverage

- Claude OAuth unified duration/scoped utilization and absolute reset headers; usage API
  5h/7d/Opus/Sonnet/OAuth windows, including explicit null and dormant values. Bare unified or
  overage retry times and monetary credit metadata are not subscription quota windows.
- Codex actual primary/secondary durations, active namespaces, additional named scopes,
  Spark/Bengalfox and code review; absolute or relative resets. API accepts wham/usage and
  normalized `codex.rate_limits` object/array additional-limit representations. Weekly primary,
  daily and arbitrary positive durations are retained. No missing secondary duration is invented.
- Unsupported providers produce no samples. Generic OpenAI RPM/TPM, Retry-After and allowed
  status alone never become subscription balances. Malformed fields are ignored independently;
  malformed supported-provider JSON returns an error. A recognized valid sibling field survives.

## Scheduler lifecycle

1. On discovering a supported account, `Queue(account, Endpoint(provider), ReasonSeed, zero)`.
   Reuse the existing DB on restart; do not clear scheduler tables.
2. Observe all normal upstream header/event signals at the internal CPA boundary, independent
   of client header forwarding. UI reads use cached windows and `Status`; a missing-data refresh
   can `Queue(..., ReasonManual, zero)` through this same scheduler.
3. A bridge worker lists `Jobs`, then calls `Acquire(account)` for eligible accounts. It must
   call Acquire even if a prior status looked ready. A nil job is **not** permission to call.
4. For a returned job, call the bridge's fixed supported quota endpoint through the host.
   Parse at the response's capture time and `Observe` successful quota evidence. Then always
   `Finish(*job, actualHTTPStatus, actualResponseHeaders)`, including partial/no-signal bodies.
   Transport failure is status 0. Preserve the original 429/status and Retry-After, not a
   synthesized generic 502. Parse/storage failure does not excuse leaving a finished call inflight.
5. Finish persists the next sparse request. No caller performs a separate direct/manual/force
   call, follows an alternative-host retry, or fetches enrichment as part of this quota job.

`due` is a base event time, with zero meaning the injected clock:

| Reason | Scheduling |
| --- | --- |
| `ReasonSeed` | base + stable 1–5m jitter |
| `ReasonDeadline` | reported reset + stable 30–90s jitter |
| `ReasonIdle` | last evidence/base + 30m |
| `ReasonActivity` | last request of a direct-only account + 5m |
| `ReasonPartial`, `ReasonManual`, custom reason | requested time, subject to every shared floor |

There is one inflight job per account **and** endpoint, at least 15m between account starts,
and 30–35s between endpoint starts (minimum plus nonnegative stable jitter). All reasons obey
these floors, including deadline and manual requests. Repeated seed/manual requests cannot
erase a pending seed's jitter. Passed-reset grace also survives manual requests.

Fresh complete evidence cancels queued fallback work and retains a `waiting` idle/deadline
wake-up. More fresh passive observations postpone it, so sustained complete passive traffic
causes zero periodic direct calls. Freshness ages balances/status after 30m; an older, future
absolute reset remains evidence. Claude completeness requires both known shared 5h and 7d
windows; explicit null counts. Codex can legitimately have only one shared weekly primary.
Every already-known scoped window must also be complete. A scoped-only signal cannot prove
main-account coverage. A successful partial observation retries no earlier than 15m; a
successful call with no stored evidence defaults to a background read after 30m.

Antigravity and OpenCode Go answer requests without quota, so only a direct read reveals their
meters (direct-only providers). Their last read covers them while it is fresh (30m); a bucket
with no running window may carry a balance and no reset. `Activity` notes each request such an
account serves; the scheduler then reads the meter 5m after the last request of a burst, at most
30m after the previous read under steady use, and always subject to the floors above. For other
providers `Activity` does nothing, because their responses carry the quota.

429/transport/other failures persist account **and** shared-endpoint not-before. Fallback is
15m, 30m, 1h, 2h, 4h, 6h, with nonnegative jitter up to 10% and a 6h fallback cap. The maximum
of fallback and valid numeric/fractional/HTTP-date Retry-After wins. Provider deadlines beyond
6h are never shortened. Sibling requests wait and then acquire one paced probe. A successful
call resets failure counters but never erases an outstanding not-before deadline. 401/403
suspend the account until explicit credential recovery calls `Resume`; UI queueing cannot
resume it. Analytics failures never modify quota evidence or inference cooldowns.

`Status` / `Jobs` include `Due`, effective `Next`, `Blocked`, account/endpoint not-before,
failure count, joined/cancelled totals and state. Inflight/authentication/endpoint-inflight
blocks have zero `Next`, meaning unknown until completion/recovery. History and Attempts
support durable metrics by source, disposition, reason, status and Retry-After. Read APIs make
no provider calls. Do not expose the acquired job's completion token as a public mutation API.

### Crash recovery

Acquired tokens and starts persist. `Open` does **not** expire or clear inflight jobs: a second
store may simply be another live consumer. After a process restart, the owner must establish
that old transports have stopped, then call `RecoverInterrupted` on each interrupted job from
`Jobs`. Recovery finishes it as status 0, retains request floors and applies failure backoff.
Late completions using the old token return `ErrStaleJob`. A crashing worker cannot trigger an
overlapping call through a guessed lease timeout.

## Boundaries and verification

No production adapter wiring is included in this package. Bridge/CPAMP/OpenCode callers must
actually share it for cross-client budgets to hold. Capture-time coverage, account verification,
supported endpoint registration, matching-scope inference cooldown recovery, and removal of
disabled accounts from views remain integration responsibilities. The store preserves history
for disabled/removed accounts; the bridge filters its active account list.

The default intervals are fixed conservative policy constants in this version; optional 30m
outside-use reconciliation is not configured here. Explicit quota snapshots with omitted
unknown scopes do not prove those scopes were removed. Codex null windows without duration
cannot be assigned an invented duration. Unsupported provider adapters remain a separate task.

Tests use real file-backed SQLite, independent pools, deterministic source clocks, 20 concurrent
consumers, restart/recovery, persisted backoff, long Retry-After, partial/no-signal/full fixtures,
nanosecond timestamps, conflict chronology, and the recorded Codex October 2 → 4 → 7 reset dates.
Intermediate balances in that historical replay are labelled synthetic; final 93% is recorded.
