# Optional last-applied marker TTL evidence

This independent package supplies readback of the **last recorded, verified,
successful native Claude dispatch's caller/CPA-controlled marker TTL**. It fixes
the distinction between a selected TTL and a TTL actually sent at the executor
boundary. It does not turn partial prefix telemetry into cache residency, cache
refresh, warming readiness, or a claim about every provider-added write.

## API and trust boundary

```go
Open(db *sql.DB, clock policyevidence.Clock) (*policyevidence.Store, error)
store.Observe(ctx, record) (policyevidence.ObserveResult, error)
store.AugmentAuthorized(ctx, callerDigest, stateJSON) ([]byte, error)
```

- `Open` borrows the caller's SQLite database. A nil clock means `time.Now`.
  Owner controls durability, busy timeout and close; one shared connection works.
- `Observe` accepts a `measurement.Record` produced by `measurement.FromNative`,
  **after** `measurement.PutRecord` succeeds. It is a minimal optional projection,
  not an alternate ingestion, credential verification or policy authorization path.
- `AugmentAuthorized` accepts only a fresh canonical policy state returned by an
  already-successful, capability-authorized **GET** RPC. Pass the same host-derived
  bare caller SHA used by the policy adapter. No double hashing, client-supplied
  caller identity or guessed-session authorization is supported.
- The package has no HTTP routes or capability API. The method name documents a
  caller obligation, not an authorization check it can independently perform.
  Parent integration must gate RPC success, `GET`, and status 200.
- On optional readback failure, retain the canonical response and status. Never
  augment POST responses, operation receipts, errors, client-supplied state, or a
  cached augmented response. Concrete parent hooks: [INTEGRATION.md](INTEGRATION.md).

Errors contain fixed non-secret messages (or context cancellation), not SQL,
database paths, credentials, raw provider payloads or model responses.

## Output

The only added top-level fields are:

```json
{
  "last_effective_ttl": "1h",
  "last_effective_ttl_evidence": {
    "status": "observed",
    "basis": "successful_native_caller_cpa_markers_only",
    "upstream_started_at": "2026-10-08T12:00:00Z",
    "policy_revision": 3
  }
}
```

`last_effective_ttl` is omitted when unknown. An ordered but uncertain latest
witness supplies only provenance with `status: uncertain` and a fixed reason.
There is no `effective_ttl` sibling and no `cache_refresh_at`, `protected_until`,
cache hit, account, provider raw data, or warming field. The timestamp is dispatch
evidence; it is **not** a cache refresh time. Existing unknown warming state remains
unchanged. Old completed dispatch evidence has no cache-expiry timer: it is a
historical “last observed marker TTL”, not a statement that a cache is still warm.

Original state member values are retained byte-for-byte, including policy hashes,
receipt objects, number representations and opaque fields. Root additions do not
re-encode those values. Duplicate/ambiguous scope keys and malformed state JSON are
rejected. Re-augmentation replaces only this package's two fields; stale owned
fields are removed when their evidence does not apply. State input/output is bounded
to the existing 16 KiB policy response limit.

## Required evidence

A record can establish a known marker TTL only with:

1. Exact valid `(caller SHA, installation, session, provider route, context epoch)`;
   positive immutable policy revision and coherent physical-attempt identity.
   A nonempty native session ID must agree with the selected scope's session ID.
2. `cpa-native` attribution, provider `claude`, builtin `ClaudeExecutor`, a resolved
   `claude-…` model identifier, and a verified native credential-binding reference.
   Actual model/route support remains the upstream policy adapter's responsibility;
   this package does not add models or infer support for other executors.
3. Final successful dispatch, `Native.Dispatched=true`, and coherent selected and
   record kinds (`primary` or `warming`). Explicit auxiliary/compaction conflicts
   cannot qualify.
4. `StartTimeBasis=upstream`, actual start matching the immutable policy start,
   requested time ≤ start ≤ end ≤ the sampled clock, and end matching the host's
   captured requested time plus latency. Missing, admission-time, future or
   otherwise untrusted clocks cannot establish a value.
5. Positive caller/CPA breakpoint count; selected and effective TTL both known,
   supported (`5m`/`1h`), and equal.

Native token counters, normalized accounting versions, optional cache-write splits,
`Activity.PrimaryPrefix`, and `Activity.Partial` are deliberately **not** marker-TTL
proof requirements. The final executor's marker snapshot remains useful when usage
accounting is unknown, zero or absent. Conversely, positive usage/selected TTL alone
does not establish wire policy. `AllWritesCovered` is neither inferred nor emitted.

The authorized state must name the exact epoch, have an already-used active
context, and have a revision at least as new as the source snapshot. Its active TTL
must agree. Pending target TTLs and old/new epochs never substitute for that active
context. A read performed after a backward clock jump withholds future-dated
provenance without changing stored facts.

## Ordering, conflicts and durability

- The SQL primary key is exactly the five scope components above, using the host
  caller digest as supplied. Accounts are intentionally not part of the key: this
  is a context's last marker observation, not account warmth.
- Compare actual dispatch starts, never callback arrival or persistence time.
  A late callback from an old account cannot replace a newer dispatch; another
  context epoch has a completely separate row.
- Same-clock witnesses agree only when their TTL facts and policy revisions agree.
  Contradiction/unknown evidence at that clock becomes durable uncertainty; another
  delivery at the same clock cannot resolve it by winning the write race. A
  strictly newer valid dispatch can establish its own last-observed marker TTL.
- A newer successful, time-verified dispatch with unknown/mixed/mismatched marker
  TTL or no markers withholds an older known value. Failed, undispatched,
  unsupported or time-unverified records cannot order marker observations; they
  leave the prior verified historical fact alone and never create a known one.
- Immutable event and physical-attempt identities are deduplicated. Same proof is
  a no-op, including usage-only enrichment. Reusing an identity with changed marker
  proof/scope returns `ErrConflict` and preserves the original. Such a record should
  already have been refused by authoritative usage persistence.
- Scoped ignored events are remembered, including their original clock decision.
  A future-clock event cannot become eligible merely because its duplicate arrives
  after time advances or after restart. Malformed scope/identity is rejected.

Schema version 1 uses only `policy_evidence_meta`, `policy_evidence_events` and
`policy_evidence_heads`. Startup is bounded empty-schema creation/version checking,
with no history scan or migration. Observations lock metadata before checking and
writing, atomically updating the dedup ledger and latest row. Cross-pool races and
single-connection sharing are tested; reads have no write effects. Source IDs,
physical IDs and immutable proofs are hashed. Only the exact scope components,
allowed TTLs, revision/time and fixed diagnostics are stored; no source raw usage,
provider paths, tokens, account/credential references or capability is copied.

This is optional readback, not complete history. Native callbacks are still an
in-memory transport; the package starts no delivery run, acknowledges no horizon,
and makes no coverage claim. A crash between usage persistence and this projection
can leave readback unknown until a verified replay or another successful dispatch.

## Verification

Deterministic tests use temporary real WAL SQLite databases. Coverage includes
native conversion → authoritative usage persistence → marker projection; partial
prefix/absent usage; restart, out-of-order/old-account/old-epoch callbacks; duplicate
and same-clock contradictions; failed/unsupported dispatch; future/admission/missing
clocks; authorized scope/revision checks; raw-secret exclusion; byte-preserved
receipt/hash fields; read-only and single-connection/shared-pool concurrency.

Commands (using the Pacer subject's Go caches and temporary directory):

```sh
go test -race -count=1 -timeout=120s -v ./internal/policyevidence
go vet ./internal/policyevidence
go build ./internal/policyevidence
```

Receipts are under `pacer/evidence/implementation/policy-evidence-*.log`. Tests use
synthetic fixtures, not claimed live provider evidence. Parent hooks, real native
client UI acceptance and production activation are separate from this package.

2026-10-08 package handoff: all three commands above passed on `pacer-policy-evidence`
(base `5428dec`). Race receipt: `policy-evidence-race.log`; successful vet/build
receipts: `policy-evidence-vet.log` and `policy-evidence-build.log` (empty output,
exit status 0). The earlier non-race receipt is `policy-evidence-initial.log`.
All implementation, tests and integration notes are new files in this directory.
