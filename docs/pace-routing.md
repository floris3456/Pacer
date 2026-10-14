# Pace routing

Status: **design**, branch `pace-routing`. Replaces the dry-run router (`internal/routing`) and its
activation gates. By the owner's decision there is no dry-run phase: routing is `off` or `on`.

## Goal

Empty every 7-day meter by its reset, while keeping each account's 5-hour windows usable for as long
as its 7-day reset allows. The 7-day meter is the binding limit: measured on all four Claude accounts,
one full 5-hour window costs 13–17 % of the 7-day meter (mean ≈ 14.5 %), so a 7-day budget covers about
7 of the ~34 five-hour windows in a week. 5-hour windows are simply used; only the 7-day meter is paced.

Slightly suboptimal routing is acceptable. CPA's own fill-first routing drains accounts in a fixed
order, regardless of which 7-day meter expires next, and only places fresh chats.

## Why the previous router never acted

Live state on CT104, 2026-10-14:

- All 125 chats have `unknown_warmth: true`. The router treats unknown warmth as protected forever, so
  94 % of decisions were `protected` and no discretionary (`cold_move`) decision was ever made.
- A 5-hour window that reset and was not used since is `dormant` (Anthropic reports `utilization: 0`,
  `resets_at: null`). Routing treats it as an unknown budget, so a fully available account is excluded.
- Account size, 5-hour value and warm-burn inputs depend on a dollar-per-percent calibration that was
  unknown for every reset in the dry-run. Pace routing uses meters only.

## Inputs

Per CPA-offered candidate (same provider, enabled, not cooling; CPA's list stays authoritative):

| Input | Source |
| --- | --- |
| `S`: 5-hour % left | `all` 5-hour window; `dormant` counts as 100. Claude requires it; a provider without one (Codex) skips the 5-hour conditions |
| `L`: 7-day % left, `R`: 7-day reset | `all` 7-day window, absolute reset |

A meter is usable when its balance is at most 1 h old (the scheduler reads idle accounts every 30 min
and 30–90 s after each reset). A passed 5-hour reset without a confirming reading is not usable.
Nothing else: no prices, no calibration, no expected request value.

## The rule

1. **Eligible**: `S > 2` and `L > 1`. A new placement also needs `S ≥ 15`, so that a new or moved chat
   does not hit its 5-hour limit right away.
2. **Pace** `= L / max(1 h, R − now)` in 7-day % per hour: how fast the account must be spent to reach
   zero at its reset. Highest pace wins. An account whose meter would otherwise expire unused has the
   highest pace; an account that is low with a distant reset is kept for later, so its 5-hour windows
   remain usable.
3. **Band**: every eligible account with pace ≥ 90 % of the best is equivalent. Within the band prefer,
   in order: the chat's current account; an account whose 5-hour window is running (its capacity
   expires at its 5-hour reset); the earliest 5-hour reset; the lowest auth ID. A dormant window comes
   last, because unused dormant capacity does not expire. This empties running 5-hour windows and avoids
   churn between equivalent accounts.
4. **No usable input** for any candidate, or no eligible candidate: `Handled:false`, CPA decides.

Example, 2026-10-14 20:00 UTC (live meters):

| Account | 7d left | 7d reset | Pace |
| --- | --- | --- | --- |
| `ae0a…` | 13 % | in 7 h | **1.86** |
| `70f0…` | 86 % | in 159 h | 0.54 |
| `1655…` | 48 % | in 98 h | 0.49 |
| `a41a…` | 60 % | in 123 h | 0.49 |

`ae0a…` takes every new or cold chat until its 5-hour window is used up or its meter expires. Then
`70f0…`, `1655…` and `a41a…` are all within the band. They are filled one running 5-hour window at a
time, and none drains its 7-day meter days before its reset.

## When routing happens

Pacer decides only when the prompt cache is cold on every account, or the current account cannot serve:

| Moment | Decision |
| --- | --- |
| New chat | rule |
| Primary request on a cold chat: idle past its confirmed TTL; with unknown warmth, 1 h after its last primary request (the longest Claude TTL) | rule; staying is preferred within the band |
| Compaction request whose model differs from the chat's model (cache is per model) | rule; only the compaction is routed, the binding is unchanged |
| First primary request with a new checkpoint (after compaction) | rule |
| Current account not offered, or below a floor | rule, as a forced move; declared clients keep the `409 quota_compact_required` contract |
| Anything else (warm, in flight, same-model compaction, title, generate) | warm chats are pinned to their account (`Handled:true`, current auth); auxiliaries `Handled:false` |

A cold move needs no compaction and costs nothing extra: the cache must be rebuilt on any account.
Pinning warm chats also stops CPA from moving them.

## Safety without dry-run

- Every refusal path falls back to CPA (`Handled:false`); missing evidence is never zero usage.
- Only CPA-offered candidates are chosen; Pacer never resurrects a removed credential.
- Setting `off` through management (persisted, no restart) restores CPA routing for the next request.
- The decision trace (`pacer_decisions`) keeps each decision with its reason and the account CPA
  served; it will also record every candidate's pace, 5-hour and 7-day input.
- Success is read directly from the meters, with no simulation: the 7-day % left at each reset per
  account (target ≈ 0), the number of forced moves, and the requests for which no account was eligible.

## Changes

1. Dormant windows. Routing treats a dormant window as known, with 100 % left (`observation.go:23`).
   The usage page (`usage_live.go:126`) and the measurement store (`measurement.go:255`) keep the
   reading as 100 % left without a reset, and the measurement store closes the previous cycle.
   - Codex: a meter-read (API) window with 0 % used whose reset is within 10 s of read time + window
     length has not started. Codex reports it as resetting one full window after each read; the
     27 such reads on CT104 were 0.1 s from that, while started windows were at least 52 min away.
     The parser stores it as dormant, like Claude's `resets_at: null`, keeping the reported reset in
     the status evidence. Response headers and stream events never qualify: a request starts the
     window, and its first minute also reads 0 % used. A window that started within 10 s of a read is
     briefly dormant until the next reading. Stored history is not rewritten.
   - A cycle replaced by a dormant reading is retired, as on any other cycle change.
   - Cooldown recovery (`recovery.go:57`) accepts a dormant window read after the failure as 100 %
     left. Today it refuses it, so an account whose 5-hour window reset unused stays in CPA cooldown.
2. `internal/routing`: replace `Pick` and its scoring with the rule above. Remove `Size`, `ShortValue`,
   `WarmBurn`, `RunwayUnknown`, `MoveMargin` and the fallback ladder.
3. Warmth: with unknown warmth, a chat is cold 1 h after its last primary request.
4. `pick`: route compaction claims whose model differs from the binding's model; treat a primary
   request with a new checkpoint as cold. Auxiliary admission still never rebinds.
5. Settings: `routing.mode` (YAML and management) accepts `off` or `on`; remove `routingEligible` and
   the activation gates. Remove the dry-run report, evidence ledger and mode marks as a separate cleanup.
6. Decision trace: record each candidate's pace and meters.

## Keep windows running

Most windows start their countdown at the first request after a reset. Measured on CT104: Claude 5-hour,
Codex 7-day, Antigravity 5-hour and 7-day (Gemini and Claude/GPT pools), OpenCode Go 5-hour. Claude 7-day
and OpenCode Go weekly/monthly run on a fixed schedule. An idle, unstarted window loses time: its next
reset comes as much later as the window stayed unstarted (on 2026-10-11 Codex sat unstarted from
03:59 to 09:34, so its weekly reset moved 5.5 hours later). Capacity is not prorated, so starting a window as soon as it resets never costs quota.
At worst an unused window expires, as it would anyway.

When Pacer reads a window as dormant, it sends one minimal request on that credential:

- **Transport**: `host.model.execute` with `auth_id` (CPA locks execution to that credential),
  `forced_provider`, `stream: false`. CPA handles token refresh and provider specifics. On Claude
  OAuth it adds only its one-line Claude Code identity and billing header. The host skips Pacer's own
  router and interceptor, so no chat, binding, routing decision or usage-ledger request is created.
- **Request**: the user message `Reply with ok`, no system prompt, no tools, thinking off, the provider's
  minimum output cap. The model is set per provider and pool (the cheapest model in that pool, for
  example Claude Haiku). It must be in CPA's model catalogue; otherwise the pool is skipped and logged.
- **Result**: Claude and Codex reply headers carry the new window; Pacer observes them as an ordinary
  reading. For Antigravity and OpenCode Go, Pacer queues an immediate meter read.

Conditions:

1. The window is dormant (change 1), from a Pacer meter read. Deadline reads run 30–90 s after a known
   reset; idle reads every 30 min catch early resets.
2. The credential is enabled, not cooling down, and no other window of the same pool is exhausted (a
   refused request starts nothing).
3. One request per dormant window. If a successful request leaves the window dormant, that provider/pool
   is not started this way again until restart, and it is logged. A failed request is retried at the
   next idle read, at most 3 times per window.

Setting: `keep_windows_running` (YAML and management, live), a list of providers; empty means off. It is
independent of `routing.mode`. Each attempt is recorded with credential, pool, model, status and the
reading that followed. With every window started immediately, pace routing's tie-break on the earliest
5-hour reset picks the window that expires first.

These are automated requests on subscription accounts, so the provider sees a request at every reset.

## Verification

- Unit tests for the rule: pace order, band tie-breaks, dormant windows, floors, the new-placement
  minimum, stale meters, and no eligible candidate.
- `Plugin.Handle` tests: a warm chat is pinned; unknown warmth stays pinned until 1 h; a
  different-model compaction is routed; a same-model compaction is pinned; the first primary after
  compaction is routed; a forced move keeps the 409 contract; `off` returns `Handled:false`.
- Keep-alive tests: dormant → one request with `auth_id`; started window, header source, cooldown,
  exhausted sibling window, unknown model, or setting off → none; still dormant after success → pool stopped.
- Isolated lab run (`tests/lab/`) before deployment; afterwards, the 7-day % left at each reset and,
  per keep-alive, the time from reset to a started window.

## Open points

- The band (90 %) and the new-placement minimum (15 %) are accepted starting values.
- Codex: the rule applies unchanged (only its 7-day window exists, so the 5-hour conditions are
  skipped), but with one Codex account there is nothing to choose. It is worth enabling once a second
  account exists. Its idle-window reads become dormant (change 1).
