# Native source coverage hooks

Built on `4b488c5` and the durable [usagecoverage contract](../usagecoverage/README.md).
These hooks consume schema-7 `SourceEvidence` and `host.usage.checkpoint`; they do not enable
measured capacity, physical source coverage or active routing.

## Wired paths

- **Startup:** `openMeasurements` opens the measurement store and creates a fresh
  `usagecoverage.Consumer` on the same FULL/WAL SQLite database. The existing stable
  `pacer_usage_source` remains the event/source namespace. Every consumer object has a new durable
  incarnation. Native startup no longer creates a legacy global delivery run.
- **Discovery:** `host.auth.list` plus `host.auth.get` produces an exact `(provider, AuthID,
  AuthIndex, source-kind, version)` set, grouped only by actually supplied billing metadata.
  `credential.AccountVerified` is true only for a nonempty bounded `account_uuid` (Claude) or
  `account_id` (Codex). Credential/name fallbacks never set this flag. Tokens are not retained.
- **After interception:** the selected credential's `measurement.NativeCredential` is inserted
  immutably in the existing `pacer_usage_credentials` row. A new additive
  `pacer_usage_capture_metadata` table stores version, original snapshot ID, mapping verification,
  exact-identity flag and capture observation time. Both inserts are one transaction. A retry with
  another account/index fails; a retry with identical credential identity keeps its first snapshot.
- **Acknowledged usage:** `Plugin.Handle(usage.handle)` loads only the durable capture by
  `(LifecycleRequestID, AuthID)`; it never falls back to current inventory. It calls
  `Consumer.ObserveUsageWithRecord`, then passes **that same accepted record** to optional policy
  evidence. Unmanaged usage works without CachePolicy when the after hook captured attribution.
  Actual dispatch/request/final observation clocks are preserved by `FromNative`.
- **Background:** the existing worker's `refreshMeasurements` calls `refreshSourceCoverage`.
  Every RPC uses the pinned membership/tuple, outside `p.mu` and the consumer's lock. A sorted
  round-robin pass makes at most eight synchronous calls, one outstanding at a time. TryLock prevents
  overlapping passes. The native Host transport owns its RPC deadline; the adapter does not spawn
  an unbounded timeout goroutine. Shutdown is checked before each call.
- **Native measurement:** every query explicitly sets `RequireSourceCoverage:true`, including
  empty queries or an unavailable consumer. When available, `BeforeMeasure` copies the exact
  AccountID→membership selections. No native hook calls global `Checkpoint` or substitutes unscoped
  `host.usage.delivery`, callback time or empty-queue state.

## Honest discovery snapshots

`host.auth.list` is **not a transactional, revisioned account-membership history**. Therefore every
snapshot made by these hooks has **`Complete:false`**. It is a zero-width point observation
(`From == Through`), never a fabricated historical interval. Measurement now permits zero-width
snapshots only when incomplete; complete zero-width snapshots remain invalid.

Unchanged tuple/mapping sets reuse their persisted snapshot via an indexed head lookup. A changed
set gets a higher local revision/new ID and faults the previous account selection. The local revision
orders our observations; it is not a host-certified inventory revision. A previously verified tuple
cannot be rebound to another account in the source-binding table. Such changes keep coverage unknown;
they cannot remap old records. Missing host ID cannot create an exact source tuple merely because a
filename looks like an ID. Unsupported custom gateways remain outside discovery's first-party set.

The integration bounds the entire discovery list to 128 entries (and 1 MiB JSON), including entries
it later excludes. Duplicate IDs/indices, limits and malformed/unavailable lists fail closed. Scoped
groups are cleared, verification is removed from subsequent captures, existing captures stay intact,
and the failure is durable. Missing individual billing metadata yields only an unverified fallback;
the old affected group is invalidated when replaced. No override promotes incomplete inventory.

`AccountVerified` in the capture table means billing metadata was observed at capture. It does **not**
mean interval membership was proven. Consequently `Record.Native.BillingMappingVerified` remains
false for these point snapshots: the consumer requires a verified mapping that spans the request's
start time before setting its stronger interval-qualified flag. The original snapshot reference is
still retained for audit. This is deliberately conservative rather than turning a discovery timestamp
into a coverage watermark.

## Errors, ordering and cached reports

Missing logical correlation, absent capture, changed qualifiers, malformed supported usage,
unrecordable counters/clocks/identity, conflicting duplicates and SQL errors return
`pacer_usage_unavailable` to the acknowledged native caller. They persist an account fault or,
when account correlation is unavailable, a consumer-wide fault. A failed invalidation latches the
consumer failure until a poison marker can be persisted. There is no nil-success drop of a tracked
supported-provider record. Unrelated non-Claude/non-Codex callbacks without SourceEvidence remain
outside Pacer's native ingestion contract; unsupported callbacks carrying source evidence refuse.

Legacy bare NativeCredential captures are still readable. Absence of the new metadata stays unknown;
a repeated after hook does not upgrade old rows using present inventory. Legacy records without
SourceEvidence remain useful stored evidence. They do not obtain scoped proof.

Checkpoint RPC/decode failures call `CheckpointFailed`; malformed/mismatched responses are refused.
The consumer persists gaps before checking stale sequence, including truncated `invalid_from` facts.
An old reply can revoke proof; a later successful reply cannot clear sticky gaps. Storage failures
propagate to the consumer failure latch. Reload never borrows an old consumer's horizon, even when
the point membership is reused. Equal/regressed startup clocks now register a **poisoned** new
consumer at the previous lower bound: ingestion remains available while coverage is unknown.

Every usage result, discovery change and checkpoint result clears cached measurement reports and
increments a generation. A concurrent report refresh only publishes if its captured generation is
still current. Thus cached reports cannot silently outlive a late invalidation. Private diagnostics
contain bounded counters and fixed reason codes, never AuthID, capabilities, host bodies or ratios.

Delayed usage-header observations also use the immutable captured account. They no longer remap
through `p.creds` after a rotation/reload. Existing capture-clock/countdown rules are preserved.

## Measurement and projection limits

Even valid individual host proof cannot close account membership. Native coverage remains unknown
with `incomplete_account_membership` (or missing membership) and `physical_carry_in_unproven` when
a scoped snapshot is present. Request-start proof does not close unobserved physical carry-in or
establish absence of external provider-account activity. No physical-coverage setter or trust toggle
was added. Counter quality, prices, provider observations and prefix proof remain separate gates.

Optional policy projection uses the accepted `FromNative` record including capture provenance and
final source clocks; it does not reconstruct identity. Projection errors remain optional diagnostics,
as before, while authoritative record errors reach the host. Selected 5m with reported 25 5m / 75 1h
writes remains exactly 25/75. Source evidence cannot invent managed primary-prefix evidence for an
unmanaged request.

## Offline verification

`coverage_test.go` invokes real `Plugin.Handle` with fake Host callbacks and an atomic controlled
clock. The background entry is driven directly for deterministic ordering. Cases cover captured
unmanaged usage, duplicates, mixed writes, exact optional projection identity, missing correlation,
malformed callback fields, SQL record/gap failures, equal-clock reload, legacy captures, delayed
account/header attribution, aliases/rotation/discovery failure, gateways, callback failure/staleness/
truncated gaps, bounded sorted polling outside locks, concurrent usage during an RPC, and empty
native queries surrounded by legacy global coverage. Measurement tests cover incomplete point
snapshots and regressed startup clocks.

Receipts and final commit: `pacer/evidence/implementation/coverage-hooks/README.md`.
No native/provider/lab calls or runtime reload were performed for this work.
