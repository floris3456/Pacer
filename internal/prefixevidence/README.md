# prefixevidence

Optional, per-attempt evidence that a single named cache entry was written by a
dispatch. It turns a Pacer **fact** (the final-wire prefix snapshot, selected
policy scope and cache-creation counters of one dispatch) and the CPA **settlement**
(terminal physical outcome of the same attempt) into an immutable observation.
It then exposes at most one `prefix_cache_evidence` object on an authorized policy GET.

Spec: `pacer/evidence/implementation/prefix-warmth-implementation.md` §S3–S5.
Pattern: `internal/policyevidence`.

## What it does not prove

- No cache residency, full-prefix coverage, remaining prefixes or warming readiness
  (`coverage` is always `named_entry_only`, `remaining_prefixes` always `unknown`).
- No effect on request routing, dispatch, policy hashes, receipts or POST responses.
- No content: the package never sees prompts, responses, tool names, headers or keys.

## API

```go
s, err := prefixevidence.Open(db, clock)            // db is borrowed, never closed
err = s.Observe(ctx, fact)                          // after the Pacer record is durable
err = s.Settle(ctx, attemptID, prefixevidence.OutcomeSucceeded) // CPA physical finish
err = s.Sweep(ctx)                                  // age/overflow bounds (also run by Observe/Settle)
body, err = s.AugmentAuthorized(ctx, callerSHA, stateJSON)
obs := prefixevidence.Qualify(fact, &outcome, anchor, now) // pure, no store
```

- `Observe` and `Settle` may arrive in either order. An identical repeat is a no-op.
  A different fact or outcome for the same attempt quarantines that attempt
  (`uncertain` / `conflicting_evidence`), returns `ErrConflict`, and never replaces
  the earlier value.
- `Qualify` applies the S3 checks in order; the first failing check gives the reason.
- A success with a warming kind needs a primary anchor with equal account, binding,
  key_id, scope, epoch, endpoint tag and settings tag, and with a start not later than
  the warming start. Anchors are registered by dispatched primaries that succeeded.
  Late anchors re-resolve their warming dependents.

## Trust boundary

- `AugmentAuthorized` is read-only. Call it only after the policy adapter has
  authorized the caller and capability for that exact scope, on a fresh GET 200.
  Never expose it as an endpoint, and never call it on cached, failed or POST state.
- Only `prefix_cache_evidence` is added, replaced or removed. Every other state member
  keeps its original bytes. State is capped at 16 KiB, and duplicate keys or malformed
  state are rejected with `ErrInvalidState`.
- Stored columns hold opaque identifiers, tags, TTLs, clocks, revisions and fixed codes.
  Raw attempt ids, bindings and tags never appear in readback.

## Output

Observed (now before `protection_estimate_until`):

```json
{"version":1,"status":"observed_partial","basis":"unique_write_destination","coverage":"named_entry_only",
 "relation":"primary_anchor|matches_primary_anchor","context_epoch":"…","policy_revision":7,
 "primary_snapshot_ref":"<32 hex>","binding_ref":"<32 hex>","entry_ref":"<32 hex>",
 "effective_ttl":"5m|1h","upstream_started_at":"…","protection_estimate_until":"t0+TTL",
 "remaining_prefixes":"unknown"}
```

Otherwise (unverified), with one fixed reason:

```json
{"version":1,"status":"unverified","reasons":["estimate_elapsed"],"remaining_prefixes":"unknown"}
```

Reasons: the S3 codes, plus `pending_overflow`, `settlement_timeout`,
`conflicting_evidence`, `estimate_elapsed` (now at or after `t0+TTL`) and
`future_evidence` (readback clock before the start). The object is omitted when no
observation exists for the scope and epoch, when the active policy is unused, or
when the newest observation postdates the state's revision.

## Ordering, bounds and durability

- Clocks: the fact's `StartedAt` is t0. Its `Observe` clock is the reference for the
  60 s future check. The `Settle` clock is used only for pending age.
- Pending (exactly one side present) is bounded at 1024 attempts and 15 minutes.
  The oldest overflow to `pending_overflow`, and age goes to `settlement_timeout`.
  A closed attempt never promotes, even if its counterpart arrives later.
- Durable across restart. The schema is version 1 and is validated on reopen.

## Verification

```sh
export TMPDIR=/private/var/folders/qv/d3dj9fsd1kngr1sttts5z47m0000gn/T/opencodeplus \
  GOCACHE=/Users/bliss/Agent/homeserverr/pacer/.cache/go-build \
  GOMODCACHE=/Users/bliss/Agent/homeserverr/pacer/.cache/go-mod
gofmt -l internal/prefixevidence
go test -count=1 -race ./internal/prefixevidence/
go vet ./internal/prefixevidence/
```

Tests cover items 1, 4, 5 and 6 of the brief. The mutation guard
(`TestMultipleEndpointsNeverPickOne`) fails when the code picks the last endpoint.
