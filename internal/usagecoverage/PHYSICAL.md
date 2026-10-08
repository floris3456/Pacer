# Durable local-dispatch causal selections — version 1

Consumes CPA `df794f42`'s optional `physical` extension (parent CPA `5526ea12`),
schema 7 / C ABI 1. **Source-only: no native acceptance or capacity eligibility is
claimed.** The native `SourceSnapshot.PhysicalCarryInProven` flag remains false.
No timestamp query, `RequestedAt` frontier or complete-inventory flag selects this
domain. Existing source `[from,through)` semantics and native accounting are unchanged.

## Exact API and meaning

```go
// Each boundary is an exact stored PhysicalCutIdentity, not just a time/sequence.
result, err := consumer.SelectPhysical(ctx, measurement.PhysicalSelection{
    From: lowerCut, Through: upperCut,
})
// Explicit historical inspection; does not install/reactivate an old listener:
historical, err := store.SelectPhysical(ctx, sameExactSelection)
```

`PhysicalCutIdentity` requires **SourceID, ConsumerID, MembershipID, SourceRef,
ManagerID, SinkID, Sequence, At**. `SourceRef` is the existing SHA-256 reference of
the stable host namespace plus full provider/AuthID/AuthIndex/kind/source-version
tuple. Cut `At` values are exact UTC nanoseconds. `.ID()` is a deterministic hash
of all these fields. The lower and upper identities must belong to the same epoch;
the exact pair must already have a persisted host-certified edge. No arbitrary
subwindow, nearest boundary, cross-sink splice or timestamp-only fallback exists.

`PhysicalResult` reports `Version:1`, domain
`cpa.claude.http.local-dispatch-sequence`, `Known`, fixed `Reason`, exact `Selection`,
`ProofID` and ordered `Facts`. A known selection attests only recorded local host
dispatches satisfying **lower.sequence < admission < dispatch < upper.sequence**,
with a genuine observed quiescent lower cut and acknowledged host closure at the
upper cut. It inherits CPA's local-producer limitation: no remote billing end,
external activity, interval-wide billing membership, prices or token correctness.

Facts contain immutable EventID/revision and stored-body digest, original consumer,
source/manager/membership references, captured account/credential and hashed binding
qualifiers, admission/dispatch sequence, `StartedAt`, and
`StartTimeBasis:"source_physical_dispatch_observation"`. No physical end is invented.
The existing record continues to use its independently validated source
`UpstreamStartedAt`; the new physical clock may be later and must be ordered between
that observation and `FinalUsageAt`. `FinalUsageAt` remains an observation, not closure.

A known **empty** Facts list is legitimate silence in this exact local cut interval.
It does not mean zero account billing, zero external traffic or known capacity.
Partial point memberships can retain this local proof, while
`Native.BillingMappingVerified` stays false and numeric reports stay unknown.

## Listener, sequence and persistence authority

1. `Consumer.New` first durably creates its fresh source-consumer ID, then calls
   `Store.NewPhysicalProof`. Install it in the usage hook before requesting any
   checkpoints. This is the existing Pacer startup ordering; no new RPC is added.
2. The first accepted lower cut must be a host **unknown / first_quiescent_cut**
   observation with cut sequence equal to issuance sequence, zero outstanding and
   pending work, no gaps, and a valid non-pre-listener clock. A known response alone
   cannot establish ownership of an earlier host lower cut.
3. A `(source_ref,manager_id,sink_id)` has one durable consumer/membership owner.
   New listeners, membership changes and sink changes cannot borrow its anchor.
   Retired epoch positive replies are refused; historical exact edges remain readable.
4. All sequence parsing uses `uint64`, with **values outside 1..MaxInt64 refused as
   unknown before conversion to SQLite INTEGER**. No floats or rounded JSON numbers
   participate. Values above 2^53 are tested exactly. Equal-sequence identical latest
   replies are idempotent; conflicts revoke. Lower sequences never advance state.
5. `ObserveUsageWithRecord` retains the original primary PutRecord semantics and
   immutable captured attribution. Before returning acknowledgment, it additionally
   commits physical evidence joined to that persisted revision. A crash/SQL error
   between commits cannot acknowledge delivery, so the host cannot certify that
   callback. A later operation first persists the existing failure latch in both
   domains. No current-inventory lookup or physical-data upgrade of a legacy row occurs.
6. Cuts/manifests/invalidations use `Store.write`, including its current global schema
   guard. This task does not edit `store.go`, schemaVersion, global migrations, source
   time reads, forward-feed or history-report files. Parent must separately review
   integration with the forward-ledger generation/feed changes.

## Fail closed without losing accounting

Physical decoding is independent of primary native conversion. Missing, unsupported,
malformed, duplicate-key, reversed-clock or out-of-range optional dispatch metadata
persists a source-manager physical invalidation; otherwise valid usage still persists.
Untracked/failed/legacy activity stays unknown. Hash/credential mismatches that violate
the existing primary attribution contract still refuse ingestion as before.

The existing `Plugin.Handle` also performs a second typed SDK envelope decode for
headers. A structurally malformed optional value (e.g. a JSON string instead of an
object) can therefore still return `pacer_usage_unavailable` **after accounting and
physical invalidation were persisted**. This task leaves that parent-owned handler
untouched. Missing extensions and shape-valid unsupported versions do not create this
error. Tests verify retention as well as the callback result; parent may review this
separate header-parser behavior without weakening primary validation.

Checkpoint negatives are persisted before either domain advances, including stale,
equal-conflicting, truncated and unknown-version replies. Any affecting physical or
request-source gap conservatively revokes the **entire physical incarnation**; time
`invalid_from` is never converted into a dispatch sequence. Optional
`invalid_from_sequence` does not narrow this conservative revocation. Usage has no
sink ID, so a usage gap applies to all sinks of its original source manager. A late
explicit physical gap count also revokes that source manager across sink reloads.
Request-domain checkpoint digests exclude the optional extension, which has its own
bounded physical digest; optional payload changes never relabel request-time evidence.
A late old-manager callback revokes old proof, not the unrelated current manager's proof.
RPC/decode failures without an attributable origin fault the requested account of the
current consumer. Later successful replies cannot clear these rows.

Known selection rechecks every immutable revision, credential binding and physical
row, and compares the ordered manifest digest recorded at checkpoint acceptance.
A stream metadata counter detects missing physical rows before a later edge could
misread them as zero; missing primary/current rows, cuts or epoch metadata also refuse.
An unseen record behind any observed quiescent cut invalidates the source manager,
including an initial lower cut or a callback delivered through a later listener.
CPA supplies no expected per-cut record count: completeness depends on its acknowledged
delivery/admission contract, not aggregate-count inference or an unauthenticated claim.

## Namespaced schema and fixed limits

`NewPhysicalProof` creates `measurement_physical_*` **version 1** only when the entire
namespace is absent. It checks metadata/table/index existence at startup, not event
history or whole-database row counts. A partial/lost/unsupported schema refuses rather
than reconstructing known proof. Reopening a handle does not reactivate its listener.

Hard, non-configurable lifetime limits: 1,024 consumers; 4,096 streams and epochs;
16,384 dispatch rows and cuts; 8,192 proofs, gaps and faults. A selection has at most
4,096 facts. Counters reserve under the shared writer; overflow durably poisons the
physical namespace, with no eviction, TTL recovery or reset/trust API. A proof stores
a bounded manifest digest/count instead of duplicating thousands of facts each poll.
Polling can exhaust these lifetime limits; this source increment does not implement
retention/rotation or claim unattended production availability.

Read-only DB fields useful for parent native acceptance (IDs/data are synthetic in
source tests):

| Table | Inspect |
| --- | --- |
| `measurement_physical_meta` | `version`, `poisoned`, reservation counters |
| `measurement_physical_consumers` | `id`, stable `source_id`, sticky `failed` |
| `measurement_physical_streams` | `id`, origin `consumer_id`, `source_ref`, `manager_id`, `records` |
| `measurement_physical_records` | `event_id`, `stream_id`, integer `dispatch_seq`, allowlisted `body` (`PhysicalFact`) |
| `measurement_physical_epochs` | exact owner IDs, `active`, integer `last_seq`, response `digest`, `lower_body`, `last_at` |
| `measurement_physical_cuts` | deterministic `id`, `epoch_id`, integer `boundary_seq`, full `PhysicalCutIdentity` in `body` |
| `measurement_physical_proofs` | `id`, `epoch_id`, `upper_seq`, body `version/domain/selection/count/manifest_digest` |
| `measurement_physical_gaps` | `source_ref`, original `manager_id`, `sink_id` (empty = every sink), fixed `reason` |
| `measurement_physical_faults` | `consumer_id`, `account_id` (empty = every account), fixed `reason` |

Primary joins use `measurement_records` and `measurement_record_revisions` by event ID
and revision 1 (native final callbacks), plus captured `measurement_source_memberships`.
No raw credential IDs, tokens, arbitrary optional payloads or gap reason text are added
to physical tables/exports. See `third_party/cpa-sdk/PHYSICAL-PROVENANCE.json` for the two
unchanged SDK files and their source hashes.

## Acceptance status

Offline tests exercise exact selected facts, selected-zero versus missing rows,
counter/identity preservation, malformed optional fields, >2^53 sequences and signed
overflow, late entire-incarnation revocation, restarts/old-manager isolation, SQL
failures, fixed limits, partial schema, and concurrent ingestion/late invalidation.
Real `Plugin.Handle` tests use the copied SDK and a fake Host with controlled clocks.
No provider, native lab, runtime reload, API/container call or active routing is used.
Matching CPA/plugin native acceptance remains parent-owned. These causal selections
must not set `SourceSnapshot.PhysicalCarryInProven` for arbitrary time intervals.
