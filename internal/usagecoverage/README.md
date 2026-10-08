# Durable account-qualified CPA source coverage

Consumes CPA `ace073d2` / schema 7 `host.usage.checkpoint` and `UsageRecord.SourceEvidence`.
This package performs **no host RPC, provider polling, runtime changes or routing**. The parent
owns hooks and inventory. The consumer writes only the borrowed measurement SQLite database.

## Exact supported claim and remaining barrier

`Consumer.Snapshot` can return `Known:true` for an account's **request-start** interval
`[from,through)` when every tuple in an explicitly verified, complete membership snapshot has
matching durable host proof. Provider, stable host installation, AuthID, AuthIndex, source
kind/version, manager/sink/consumer incarnations and sequence are bound. Tuples are hashed before
storage; raw AuthID is not retained in native records. A's proof cannot certify B or partial aliases.

**This does not enable a native measured-capacity ratio.** CPA's frontier is on `RequestedAt`,
while measurement allocates usage by physical `StartedAt`/`EndedAt`. It does not prove absence of
unobserved carry-in: a pre-registration request can publish usage to an old sink and continue its
producer after a new consumer begins. Final usage time is not a separately observed physical
producer-end timestamp either. A later empty queue cannot resolve those facts.

`SourceSnapshot.PhysicalCarryInProven` is always false; no setter, configuration, inferred timeout
or manual trust override enables it. Scoped reports retain `physical_carry_in_unproven` and unknown
capacity/stretch/burn coverage even when request-domain proof is known. Future support needs an
independent paired physical start/end and pre-window closure invariant. Account mapping, complete
applicable provider observations, counter/price/prefix quality remain separate gates. This source
never proves absence of other installations or external uses of the same provider account.

## Parent hook contract

### Startup and caller-established membership

```go
store, err := measurement.Open(ctx, db, measurement.Options{Clock: now})
// Check err; owner controls SQLite durability, connections and Close.
consumer, err := usagecoverage.New(ctx, store, usagecoverage.Options{
    SourceID: stableHostInstallationID, Clock: now,
})
```

Create one consumer for the native sink and install it in the acknowledged usage hook **before**
requesting checkpoints. `New` generates a fresh durable ID; older consumers for the same host
become inactive. Restart/reload requires a new object. Old IDs cannot be reused. Startup does not
acknowledge history. SourceID is a stable installation namespace, not a process ID or secret.

```go
err = consumer.PutMembership(ctx, usagecoverage.Membership{
    ID: immutableSnapshotID, Provider: "claude", AccountID: immutableBillingAccountID,
    Revision: inventoryRevision, From: verifiedFrom, Through: verifiedThrough,
    Verified: billingMappingActuallyVerified,
    Complete: allRuntimeTuplesActuallyEnumerated,
    MappingEvidenceRef: opaqueVerifiedMappingEvidenceReference,
    Members: []usagecoverage.Member{{
        Scope: pluginapi.UsageSourceScope{
            Provider: "claude", AuthID: exactHostAuthID, AuthIndex: exactHostAuthIndex,
            Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1,
        },
        CredentialRef: opaqueCredentialReference,
    }},
})
```

- Flags describe actual caller-established evidence, **not user preferences**. Do not add a manual
  trust setting. If complete runtime billing-account membership cannot be proved, leave it
  unverified/incomplete; individual known host proofs cannot compensate.
- Credential-ID fallbacks are not billing accounts. Include every runtime tuple/alias, not just
  the selected credential. Missing/partial tuples are refused; empty/incomplete/unverified
  membership stays unknown. No wildcard source or inferred alias grouping exists.
- Membership bounds are a caller-verified, non-future interval. Stability is not extrapolated beyond
  `Through`. Snapshots are immutable; a higher revision/new ID cuts the previous snapshot at its
  `From`. Intervals across snapshots are not joined. Verified tuple/account/credential bindings
  cannot be reassigned. Unverified snapshots do not reserve a verified binding.
- Preserve the original captured snapshot on replay; do not join delayed usage to mutable inventory.

### Acknowledged usage: never swallow a failure

```go
func observeUsage(rawJSON []byte, captured usagecoverage.Capture) error {
    _, err := consumer.ObserveUsage(ctx, rawJSON, captured)
    return err // propagate through acknowledged usage / native RPC error envelope
}
```

`Capture` contains `measurement.NativeCredential`, `MembershipID`, and `Verified`, captured **by
value at attempt correlation time**. The verified member must match the exact source tuple,
immutable account and credential reference, with RequestedAt inside that snapshot's interval.
Mapping verification and snapshot ID become immutable sanitized native provenance.

Nil means the record and applicable invalidations were durably accepted. Missing correlation or
physical RequestID, invalid identity/clocks, duplicate conflict, unrecordable input and persistence
errors must reach the host. Never convert an unrecorded tracked callback to nil success. Invalidate
cached reports on new gap facts/errors. Legacy records without SourceEvidence are still stored
and useful for valuation; their consumer/account coverage stays unknown. Unverified attribution
can retain a record but faults coverage. Unattributable/malformed input faults the whole consumer
and returns an error. Existing immutable revision/deduplication behavior is retained.

### Separate background checkpoint worker

The parent calls `host.usage.checkpoint` for each exact member tuple using native callback-instance
context. Keep the requested tuple; do not select a scope/account from the response.

```go
err = consumer.ApplyCheckpoint(ctx, snapshotID, requestedScope, decodedResponse)
// On host RPC/decode/registration error, before reusing any cached proof:
gapErr := consumer.CheckpointFailed(ctx, snapshotID)
```

Do not self-drain within `usage.handle`. This package never waits for host/network drain. Old hosts
without this callback supply no proof; `host.usage.delivery`, last callback time, polling time and
empty queues are not substitutes.

Gap aggregation is inserted **before** sequence/proof acceptance within one SQLite transaction.
`invalid_from` survives truncated gap lists; any unknown beginning means the whole incarnation.
The earliest invalidation cannot be narrowed. This implementation conservatively makes the entire
affected proof unknown, retaining precise earliest invalidation for audit.

Stale replies persist gaps, then return `measurement.ErrStaleSource`. Identical latest replies are
idempotent. Same-sequence conflicts and bound regression invalidate before refusal. Host/sink
retirement and sequence tracking are independent of membership and consumer snapshots: a fresh
membership cannot reactivate an old pair. Usage-record gaps apply to all sinks for their source
manager; host checkpoint gaps are sink-qualified. No successful later reply erases a sticky gap.

A storage failure latches an in-memory failure. Before the next consumer operation can proceed,
it must persist a sticky consumer poison marker; until then it returns an error. Recovery cannot
restore positive proof in that object. A new consumer has a new lower boundary, while durable
source-incarnation gaps survive. Do not bypass these hooks with low-level store calls or keep
serving cached known proof after an error. Returning callback errors also makes the host record
its persistence failure. There is no silent best-effort success path.

### Native reads

```go
q, err := consumer.BeforeMeasure(ctx, measurement.Query{
    Provider: "claude", AccountID: accountID, Scope: quotaScope,
    BookID: bookID, Since: since, AsOf: asOf,
}, map[string]string{accountID: snapshotID})
if err != nil { return err }
report, err := store.Measure(ctx, q)
// Separate request-domain diagnostic proof, not a capacity checkpoint:
proof, err := consumer.Snapshot(ctx, snapshotID, "claude", accountID, since, asOf)
```

`BeforeMeasure` forces `RequireSourceCoverage`, pins the consumer and copies the explicit
AccountID→membership map. Measurement reads source state in the same SQL snapshot as usage and
observations, **without reading or OR-ing legacy global runs**. Population stretches from other
accounts require their own map entries. Native records also defensively force scoped mode if the
caller forgets the hook; the parent must still use it for empty queries where no records identify
the intended mode. Synthetic/global APIs remain separately backward compatible.

Scoped proof never calls global `measurement.Checkpoint`/`StartDelivery` or writes their tables.
Known source delivery does not create price, counter, model, prefix or provider-window evidence.

## Native clocks and sanitized evidence

`FromNative` decodes PascalCase `SourceEvidence` and snake_case nested fields, checks exact
credential qualifiers and clock order, and stores a detached typed projection. Raw AuthID,
arbitrary gap reason strings, headers, secrets and full callback bodies are excluded.

`UpstreamStartedAt` supplies actual dispatch even unmanaged. For managed requests it cannot precede
the earlier policy-dispatch clock. `RequestedAt` remains the coverage-domain clock. `FinalUsageAt`
becomes `EndedAt` with `EndTimeBasis:"source_final_usage_observation"`, not a physical-close/billing
claim. Missing old clocks retain `requested_at_unverified`. No callback clock substitutes for one.
UTC nanoseconds and exclusive upper bounds are preserved; coarse storage must round inward.

Selected **5m with reported 25 5m / 75 1h writes** stays 25/75. Missing counters remain missing.
Source proof does not invent prices, writes or primary-prefix evidence for unmanaged requests.

## Storage bounds and verification

Measurement schema 1 upgrades additively to 2 under the existing immediate writer lock: empty
source tables/indices and metadata only, no scan/rewrite/repricing of historical usage. Schema 2
refuses missing tables. Existing data and global APIs remain. The owner controls SQLite durability.

Caps: 128 tuples/membership, 128 account selections/query, 16 wire gaps, 16 KiB encoded checkpoint;
1,024 consumers, 4,096 memberships, and 8,192 each of verified bindings, runs, epochs, gap aggregates
and account faults. Counters enforce caps without history scans. Reads use primary/unique keys
bounded by explicit membership. Overflow fails closed. No time-based repair, eviction or reset API.
Native records retain their existing independent storage/analysis bounds.

Offline tests use local WAL SQLite, controlled clocks and deterministic concurrency. Coverage includes
restart, aliases/membership cuts, account isolation, nano bounds, late/truncated/unknown-start gaps,
stale/duplicate/error replies, database failures, limits, unmanaged clocks, carry-in refusal and
mixed-write preservation. Evidence: `pacer/evidence/implementation/source-coverage/README.md`.
