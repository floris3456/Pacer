# Usage Quota: one continuous record (phase 2 design)

Status 2026-10-09: **design, not implemented.** Phase 1 (`usage/*` routes, model
mix, Inspect, exclusions, removal of the frozen-history and legacy views) reads
only the prepared facts, which end at the import boundary H. After the
production swap, cards would stop at H. Phase 2 continues every profile, window
and filter from H to now with the same arithmetic, so the page never shows a
"frozen" vs "new" split.

## Rule

One engine, one window list per account. Before H the inputs are the imported
CPAMP events and meter readings. From H on, the inputs are Pacer's own recorded
requests and meter readings. A window is computed once from whichever inputs
fall inside it, whichever side they come from, with CPAMP's own derivation and
normalization (`Derive`, `deriveValuationWindows`, `normalizeProfileWindow`). The
user chose this (2026-10-09, "same as history"): `$ per 1%` = recorded value
between two meter readings ÷ meter change, shown as a range. When Pacer's
stricter capacity check (C) also passes, the window gets a "verified" mark; it
never replaces the number.

## Inputs after H

| Input | Source | Conversion |
| --- | --- | --- |
| Requests | forward store `forward_native`, final revisions, `started ≥ H` | the forward event → `cpamp.Event` converter the forward report already uses |
| Meter readings | `observation_history` samples (header and API) | `Start = reset − seconds`, `Reset`, `Duration`, `Used = 100 − remaining`, `Observed = sample time`, `Kind` from the duration (5h/weekly), scope map below, `ID` = row id prefixed `native:` |
| Accounts | forward origin certificates (`LegacyAccount ↔ NativeAccount`, dated) | a certified native account continues its legacy account ID; others get `native:<account digest>` |
| Profiles | forward profile config (seeded from the frozen config at H, then user edits) | same IDs, so exclusions and cards continue |
| Prices | frozen comparison book + registered forward book | unknown models stay "missing price", never guessed |

Scope map. Real CPAMP data (2026-09-01 → 10-08) has Claude `5h|all` and
`weekly|all` from the `anthropic-ratelimit-unified-5h|7d` headers, which Pacer
parses to scope `all` as well. Codex (`codex-main`, `model:gpt-5.3-codex-spark`),
Claude `7d_oi` (Pacer: `fable`), the Opus/Sonnet API pools and Antigravity are
**not yet mapped**: each needs a real overlap (the same reading seen by CPAMP and
Pacer) before it continues an imported pool; until then it is its own pool.

## Windows that straddle H

The window open at H gets its pre-H part from the artifact: Pacer loads the
artifact's events and readings for `[H − 7 days, H)` once at start (bounded:
one week). The native build replaces the frozen copy of any window that resets
after H; windows that reset at or before H always come from the frozen facts.

## Keeping it instant

- **Sealed windows.** A window that reset more than one hour ago, with no newer
  readings or requests inside it, is stored as an immutable facts object in the
  forward store (window facts + its bucket rows, keyed by window ID and an input
  digest). It is never recomputed.
- **Open work.** Each build reads only inputs at or after the oldest unsealed
  window's start (normally the last 7 days per account). A build runs after the
  forward publisher seals new requests or a reading arrives, at most once a
  minute, outside the two report slots, and swaps the in-memory facts
  atomically. Reads stay ~1 ms.
- Buckets (account × UTC day × model × key × profile set) for days before the
  oldest unsealed window are sealed the same way.

## Live meters on the cards

`profile.current` comes from the newest native reading per member account (via
the certificates), not from the facts at H. A reading older than its pool's
reset is shown as "stale".

## Tests and acceptance

- Frozen-only input reproduces the current facts exactly (no drift at H).
- Synthetic native fixtures: a window straddling H, a certified and an
  uncertified account, an unknown-price model, a reset inside the open range,
  sealed windows surviving a restart, exclusions on native window IDs.
- A rebuild after N new requests touches only the affected accounts' open
  windows (counted in the test).
- Lab: the existing forward fixture (108 native requests) plus native readings
  show at least one native window with a rate on its card; then the real-data
  drill for timings.
