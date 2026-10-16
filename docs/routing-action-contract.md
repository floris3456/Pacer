# Gated scheduler action execution

Worktree `active-routing`, branch `pacer-active-routing`, base `3130ef3`.
Implements the effect of a routing decision and tests it through `Plugin.Handle`, the
JSON dispatcher called by `cmd/plugin`'s native ABI entrypoint.

> **Superseded** by [pace-routing.md](pace-routing.md): the activation gates and
> `routingEligible` are removed; routing is `off` or `on`, and `routingAction` no
> longer takes a mode or eligibility. The forced/cold contract below still holds.

**Activation and observation gates are UNMET.** Runtime YAML and management still
accept only `off` / `dry-run`; persisted `active` is still refused, and management
still reports `can_activate:false`. PLAN's external integration/rollout acceptance
and 24–48 hours of dry-run observation (at least 24 hours) have not been certified.
There is no timer, YAML override, environment variable, public setter or acceptance
heuristic. `New` leaves the private `Plugin.routingEligible` flag false. Only the
new `_test.go` fixture assigns it true and injects `cfg.Routing.Mode = "active"`.

## Exact helper API

In `internal/pacer/routing_action.go` (package-private):

```go
func routingAction(
    req pluginapi.SchedulerPickRequest,
    mode string,
    eligible bool,
    d routing.Decision,
    candidates []credential,
) pluginapi.SchedulerPickResponse

func selectionIntent(
    t attempt,
    actual credential,
    model string,
) tracker.Selection
```

`credential` is the existing value-only inventory record (`ID, Index, Provider,
Account, Alias string`). Copy candidate records from `p.creds` while holding
`p.mu`; never hand these helpers a live inventory map. The helpers do no I/O,
polling, policy mutation, candidate sorting or rewriting of host-owned metadata.

`routingAction` returns **only** `{Handled:true, AuthID:d.Auth}` when:

- mode is exactly `active` and explicit eligibility is true;
- model, handled decision, auth and account are present;
- primary/inferred provider is supported (`claude` or `codex`), all entries in
  `req.Providers` and all offered candidates agree with that provider;
- every offered candidate has a unique ID and a known discovery/account binding;
- the proposed auth is in this invocation's candidate list, and its discovered
  account matches the decision.

Every refusal is the zero response (`Handled:false`): CPA retains its fallback
behavior. No credential can be resurrected from discovery after CPA removed it
from the current candidate list. The caller still enforces `cfg.Enabled`, configured
provider allowlisting, authenticated tracked chat identity and primary request kind.
CPA owns availability, disabled/cooldown filtering, actual provider/model resolution
and final cache-policy enforcement. Its offered candidates are authoritative.

`selectionIntent` starts with `Forced:true`. It returns `Forced:false` only for an
emitted, handled, non-rejected/non-delegated **`cold_move`** whose nonforced decision,
auth, account, provider and model match the actual host selection and the saved
request proposal. The prior account and scoped chat key must be present, and the
request must be primary. Matching an advisory dry-run proposal alone is insufficient.
CPA choosing a different credential alias, account or model retains the forced
contract. A forced decision retains it even when CPA follows the proposal exactly.

## Narrow parent integration hunks

The worktree includes narrow edits to `internal/pacer/plugin.go` so the tests exercise
the actual `Plugin.Handle` path. A standalone copy of those hunks is in the external
receipt folder's **`plugin-integration.patch`**, for the parent editing that file
concurrently. It is the same integration, not an additional patch to apply twice.

Hook points (use symbols/context rather than line numbers on a changed parent):

1. Add private `Plugin.routingEligible bool` with no runtime assignment. Add
   `attempt.RoutingChoice pluginapi.SchedulerPickResponse` and
   `attempt.RoutingProvider string`.
2. At the **start** of `pick`, under `p.mu`, snapshot configuration/eligibility and
   tracked request, then clear the saved `RoutingChoice` and `Decision`. This must
   precede **all** early returns, including mixed/empty/unknown retry candidates.
   Otherwise an old cold proposal could waive a later forced-move checkpoint.
3. Keep existing configured-provider, scoped-identity and auxiliary guards, and
   check `cfg.Enabled`. Copy known discovery records into `[]credential` in the
   existing candidate/observation loop, before an observation read can fail.
4. After `routing.Pick`, save `Key`, `Decision`, `Model`, `PreviousAccount`, provider
   and `routingAction(req, cfg.Routing.Mode, eligible, d, candidates)` together on
   the attempt. Return the saved choice, rather than unconditional zero.
5. In `after`, pass `selectionIntent(t, c, req.Model)` to `p.chats.Admit` in place
   of the unconditional `tracker.Selection{..., Forced:true}`. Keep the existing
   claim, `compact_on_switch` gate, 409 handling, usage attribution and cleanup.

The parent must preserve the zero eligibility flag until the separately approved
rollout work supplies authorization. The test fixture is not runtime configuration.

## Routing, compaction and cache-policy behavior

- A healthy protected 1h context remains on its account after six minutes. Unknown
  warmth and in-flight contexts also stay. A changed model alias with the **same
  declared route/checkpoint** does not create a fresh chat. Legacy unclaimed
  identities remain model-scoped; this change adds no speculative alias equivalence.
- A genuine cold move to the exact emitted target carries the same committed
  checkpoint, increments the account-binding generation and needs no compaction.
  Cache rebuild protection is unknown until qualifying activity establishes it.
- Removing the current account from CPA's offered set is a forced move: the existing
  declared auto-compaction contract returns `409 quota_compact_required` before
  upstream dispatch; retry with a newly committed checkpoint admits the new account.
- Compaction/title/generate auxiliaries return `Handled:false` and do not rebind or
  refresh the primary context. Retained same-account/checkpoint activity can refresh
  known warmth after logical completion; late old-account activity cannot warm a
  moved binding. Unknown/partial native cache evidence remains conservative.
- Managed primary **and warming** policy snapshots remain independently enforced.
  Neither a handled scheduler choice nor an off/dry-run/ineligible fallback changes
  TTL, authorizes a mismatched snapshot, clears a policy guard or permits committing
  a TTL switch with outstanding physical/logical work. CPA must continue invoking
  resolve/attempt/final executor validation in all routing modes.

## Pure-router fallback/runway audit

Additional real tests cover known-budget preference over unknown/stale budgets,
all-unknown fallback, most-room fallback excluding unbounded/exhausting runway,
retention of warm contexts, candidate ordering, and the distinction between an
emergency forced move and discretionary new/cold placement.

One defect was fixed in `internal/routing/router.go`: NaN/infinite/negative measured
short-window value or warm burn could pass comparison guards as if load were zero.
These inputs now make runway unusable for discretionary placement and fallback.
Zero remains the documented unmeasured percentage/15%-headroom fallback. A forced
emergency may still use a known-budget account with uncertain runway, preserving
`Forced:true` and the compaction gate; it never fabricates an unknown budget.

## Verification and remaining rollout work

Passed using task-local offline Go caches and deterministic real SQLite fixtures:

```text
go test -count=1 ./internal/pacer ./internal/routing
go test -race -count=1 -coverprofile=<receipt>/coverage.out -v \
  ./internal/pacer ./internal/routing ./internal/tracker \
  ./internal/policyadapter ./internal/cachepolicy
go vet ./internal/pacer ./internal/routing ./internal/tracker \
  ./internal/policyadapter ./internal/cachepolicy
go build ./...
git diff --check
```

Final race coverage: Pacer **69.1%**, router **95.5%**, tracker **79.4%**, policy adapter
**86.3%**, policy store **81.5%**. The action tests use exact vendored CPA JSON DTOs,
`Plugin.Handle`, actual tracker/policy SQLite state, and explicit private test-only
eligibility. They do not claim a newly loaded CPA process, deployed shared library,
provider acceptance, approved activation or a completed observation interval.

Receipt/logs: `/Users/bliss/Agent/homeserverr/pacer/evidence/implementation/active-routing/`.
Caches: `/Users/bliss/Agent/homeserverr/pacer/.cache/active-routing/`.
Parent integration and external rollout acceptance/observation remain outstanding.
