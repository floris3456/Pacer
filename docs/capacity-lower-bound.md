# Capacity lower bound — Pacer side of the capacity membership contract

Pacer reports a dated **lower bound** on an account's `$ per %` when membership (M) and
carry-in (K) both hold for one interval between two provider samples. It never reports
a known capacity: nothing outside this CPA using the same account (claude.ai, Claude Code
direct, another CPA) can be observed. Because that outside use only adds to the observed
%, the ratio can only be too low, never too high. The normative contract is
`evidence/implementation/capacity-membership-contract.md` (approved 2026-10-08).

## What is never used

No configured, claimed or default plan size, tier, multiplier or "known" ratio exists.
`Config` has no such field, so YAML containing one is refused at register. A missing
measurement stays `unknown`. Routing still uses `%/h · equal sizes`; the lower bound is
readback only.

## Evidence flow

1. **Sampling.** Samples come only from the observer's existing scheduled provider read,
   with no extra provider calls. For Claude, Pacer brackets that one read with local host
   reads:
   inventory checkpoint c1 → quiescent cut `a` for each Pacer tuple → provider read →
   cut `b` for each tuple → inventory checkpoint c2. Cut reads go through the same
   `usagecoverage` consumer path as the background checkpoint refresh, so cuts and
   proofs are durable. Only the shared `all` windows (5 h, 7 d) are stored. CPA value
   counts every model, so model-scoped pools would not give a lower bound.
2. **Ledger.** Inventory checkpoints (known or not), run-length tuple bindings and samples
   live in `capacity-evidence.sqlite` beside `state_path`. A separate file keeps the
   guarded state database's schema unchanged. If it cannot be opened, capacity stays
   unknown and the error is counted; startup is unaffected.
3. **Judgment** (background worker, `refreshMeasurements`). Each pair of consecutive
   samples per window is judged again on every pass:
   - **M:** `JudgeMembership(account_ref, s1.c1, s2.c2)`. Both checkpoints must be known,
     in one host incarnation, with no unknown `account_ref` tuple and no binding change
     inside the interval.
   - **K:** both samples are sandwiched for every active tuple, in one manager, sink and
     listener incarnation. The exact durable physical proof from s2's lower cut to its
     cut `b` is re-read every time, so a late gap or invalidation revokes an earlier
     result. The records counted are exactly those with
     `a1.sequence < dispatch_sequence < a2.sequence`. Each must be delivered, final and
     counter-complete. The last completed dispatch must be ≥ 120 s before s2.
   - **Ratio:** `CPA low value ÷ (Δ% + quantum)`. The quantum is the provider's
     reported precision (1 for an integer). The result is unknown if anything is
     unpriced or incomplete, the windows differ, a reset falls between the samples,
     or the denominator is not positive.

## Old hosts

If a host lacks `host.auth.inventory_checkpoint`, the inventory read fails and is stored
as unknown. Pacer then skips the cut reads for that sample, and the interval is
`unknown` with `membership_unproven` and `inventory_unknown`. This is never a startup or
measurement error. Point discovery stays `account_membership_complete:false`.

## Readback (`GET /v0/management/plugins/pacer/state`)

All of this is additive; existing fields are unchanged. `measurement.capacity_known`
is always `false`, and each account row gains `capacity`:

```json
{"status":"lower_bound","reasons":[],"exclusivity":"unproven","capacity_known":false,
 "window":"all:604800","interval_from":"…","interval_to":"…","age_seconds":30,
 "value":{"lower_bound_usd_per_percent":0.000388,"cpa_value_usd":0.001165,
          "delta_percent":2,"quantum_percent":1,"records":1},
 "basis":"sandwiched_cuts_provider_settle_assumed","history":[…]}
```

The top level is the newest judged interval; `age_seconds` is computed when the state is
read. `history` holds up to 8 earlier intervals, each with its own dates and status. A
newer interval is reported beside the older ones and never averaged into them.
`basis` labels the provider-settle assumption, which cannot be proved.
