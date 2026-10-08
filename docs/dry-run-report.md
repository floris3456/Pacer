# Daily dry-run report

PLAN §6 Phase 2: a daily report of placements and would-be moves, a flip-flop check, and
projected unused % at each reset compared with fill-first. The report only reads stored
evidence. It never calls the host or a provider, never writes the state or evidence
databases, and never changes routing. It is computed when requested and never stored.

```
GET /v0/management/plugins/pacer/dry-run-report?day=YYYY-MM-DD
```

`day` is a UTC date. It is optional and defaults to yesterday. It must be a single strict
`YYYY-MM-DD` value from the last 31 days, up to today; anything else returns `400
invalid_day`. Today's report is allowed and is marked `day_in_progress`. The report shares
the two-slot admission of the history reports, so a third concurrent call returns `429`
with `Retry-After: 1`. Unavailable evidence returns `503` with an `error` code:
`dry_run_evidence_unavailable`, `decision_state_unavailable`,
`dry_run_decisions_over_limit`, `quota_cycles_unavailable`, `usage_records_unavailable` or
`dry_run_report_unavailable`.
Responses are `application/json` with `Cache-Control: no-store`.

## Evidence and storage

- **Decision trace** (`pacer_decisions` in the state database). This is the existing
  dry-run trace. Each row now also carries `request_id` (the CPA lifecycle request) and
  `actual_account` (the billing account CPA served) inside the existing JSON body. The
  table's DDL is unchanged. Rows written before this change have no `actual_account`;
  they are counted as `unattributed_decisions`.
- **Evidence ledger** (`dry-run-evidence.sqlite` beside `state_path`, mode `0600`). It
  holds the installation's 32-byte random `chat_ref` secret and the mode marks. It is a
  separate file because the measurement feed witness refuses any DDL change to the state
  database. If the ledger cannot be opened, the route returns `503`, and startup and
  routing are unaffected.
- **Quota cycles, readings and priced usage** come from the existing measurement store.
  This uses read-only queries only.
- **Measured ratios** come from the background worker's latest measurement report. The
  route never measures on its own.

## Proof that dry-run was active

Pacer writes a mark recording the effective mode, the routed providers and the run's
cumulative decision-write failures. A mark is written every minute by a heartbeat
separate from the provider worker, and also at register or reconfigure, on a mode change
and on stop. Two consecutive marks of the same run, at most 10 minutes apart, prove that
the earlier mark's mode held in between. The mode is recorded as `disabled` when
`enabled: false` and `stopped` after quiesce. A restart starts a new run, so the gap
before the restart is never covered. An interval in which the failure count rose is an
error segment. Marks older than 40 days are pruned.

Coverage proves that the mode was in effect. It does not prove that every request was
traced: a request that never reached a proposal leaves no row. Examples are a request with
no chat key, an unresolved affinity, or an unrouted provider.

## Report

- `meta`: day bounds, `generated_at`, decision and chat counts, and per-provider
  `coverage` (covered seconds and intervals). `complete` is true only when `reasons` is
  empty. Possible reasons: `day_in_progress`, `dry_run_not_active`,
  `dry_run_coverage_partial`, `decision_write_errors`, `decisions_unattributed`.
- `placements`: one entry per provider and model. It counts proposal reasons and outcomes,
  and compares the proposal with CPA's actual account (`agree`, `differ`, `unhandled`).
  Per account it reports `proposed` (every proposal), plus `actual` (CPA, fill-first) and
  `simulated` admitted requests.
- `would_be_moves`: simulated `cold_move` and `forced_move` events in the day, with
  `from`, `to` and CPA's `actual_account`. `total` is exact. `items` holds at most 200,
  and `truncated` marks the rest.
- `flip_flops`: a simulated move back to an account the chat left at most 24 h earlier,
  returning within the day. The departure may be on the previous day, across midnight.
  Lists are capped at 200 items, like the moves.
- `resets`: every quota cycle of any account that ended in the day.

**Simulation.** Chats are replayed in time order, starting up to 8 days before the day.
A `new_chat` or move proposal puts the chat on the proposed account. A proposal to stay
keeps the simulated account. An unhandled request follows CPA's own account. Whenever the
dry-run proof is interrupted, a chat restarts from CPA's binding. At most 200,000 decision
rows are read. Older rows beyond that limit shorten the lookback, and a day that does not
fit returns `503`.

**chat_ref.** This is the first 64 bits of `HMAC-SHA256(secret, domain ‖ chat key)`, as
hex. It is stable across days and restarts for one installation and cannot be recomputed
without the secret. The chat key, caller, session and headers are never in the report.

## Resets and projection

A cycle's reset is its provider deadline, which is the cycle ID the observer assigns. The
deadline is confirmed by a later observed closure (`scheduled`) or not yet confirmed
(`scheduled_unconfirmed`). An earlier closure is labeled `observed_early`, and a closure
without a deadline is labeled `observed`.

**Baseline** (`fill_first_observed`). Dry-run never changes CPA's pick, so the observed
meter is the fill-first outcome. The baseline is the newest usable reading of the cycle at
or before the reset, reported as `100 − used`. It is `fresh` when that reading is at most
1 h before the reset, `stale` when it is older, and `none` without a reading. A stale
value is an upper bound, because usage only grows within a cycle.

**Projection** (`pace_dry_run_simulation`). For the cycle's account:

```
projected_unused = observed_unused − r × (value_in − value_out)
```

- `r` is percent per dollar: `1 / usd_per_percent` of this exact cycle's measured window.
- `value_in` is the API-equivalent value of admitted requests in `[cycle start, reset)`
  that the simulation serves on this account while CPA served them elsewhere.
- `value_out` is the value of requests CPA served on this account that the simulation
  serves elsewhere.

A request's value is the sum of its priced usage records, joined by request ID and the
account CPA served. The result is clamped to 0–100, and the unclamped value is reported
too. Every known projection lists its assumptions: `equal_quality` (a moved request has
the same value on its target account), `proposals_conditioned_on_cpa_bindings` and
`no_floor_feedback`.

Otherwise the projection is `known:false` with every reason that applies:

- `baseline_stale` or `baseline_none`;
- `ratio_unknown`, with `ratio:<reason>` such as `measurement_unavailable`,
  `cycle_not_measured` or a window's own reasons;
- `cycle_start_unknown` or `cycle_before_decision_window`;
- `dry_run_coverage_incomplete` or `decision_write_errors` within the cycle;
- `decision_unattributed`, `decision_without_request_id` or `ambiguous_join`;
- `moved_request_without_usage`, `unresolved_value` or
  `moved_request_crosses_cycle_boundary`;
- `usage_records_over_limit` (more than 200,000 usage records in range).
