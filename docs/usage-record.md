# The usage record (v3)

Status 2026-10-09: implemented (`internal/historyreports/usage_record*.go`, `usage_settings.go`, `usage_prices.go`,
`usage_pools.go`; runtime `internal/pacer/usage_live.go`, `usage_accounts.go`, `usage_settings_api.go`,
`model_prices.go`). It replaces the live build of `usage-unification.md` (phase 2), which re-derived every account
from H on every build.

## What it is

One window list per account, across the imported CPAMP history and Pacer's own record, derived by CPAMP's own
functions (`AccountFactsFor`: `Derive`, `deriveValuationWindows`, `normalizeProfileWindow`, ...). The Usage Quota page
reads its facts; nothing is recomputed per request.

## Sealed segments: closed windows are stored, never derived again

Time is cut into fixed weekly segments (Monday 00:00 UTC). For each account and window kind (5h, weekly, monthly), a
segment holds the windows whose reset falls inside it; a "day totals" segment holds the account's requests of that
week by day, model, key and cards.

A window segment is derived from a bounded evidence range: requests from one window length before the segment to one
hour after it; meter readings from twice the window length plus a day before it to one window length plus a day
after it. Every window's derivation reads only evidence inside that range (its own readings, any overlapping reset
revision, its requests and its boundary bands), so deriving a segment alone gives the same windows as deriving the
whole history. Two rules are applied at assembly because they depend on the whole history: an idle forecast window
is dropped when a later reading exists (Derive's idle rule), and "latest", "current" and "stale" are decided over
all segments at the build time.

**Proof:** on the real artifact, with CPAMP's own pool names, the record equals the prepared facts for all 377
windows, every day bucket and every credential (`TestRealRecordProbe`, opt-in).

A segment is **sealed** once its evidence range is complete (range end + 2 days). Sealed segments are written to
`usage-record.sqlite` and loaded on start. A segment is derived again only when:
- new evidence lands inside its evidence range (late revisions included);
- the price of a model it uses changes;
- the account's key changes: its logins (links, certificates), its profile intervals (`configDigest`), this code's
  version (`RecordVersion`, `PoolsVersion`) or the imported artifact.

Open segments with new evidence are derived again within 30 s (5-hour windows, day totals) or at most every 2
minutes (weekly) and 10 minutes (monthly); resets and sealing are never delayed.

Real data (55k requests, 108k readings over the replayed weeks, local):

| | time | allocated |
|---|---|---|
| first build of the whole record (imported history) | 14 s | ~14 GB in total (once; then sealed) |
| fresh process, everything sealed | 0.3 s | 16 MB |
| live update, busiest Claude account, 5h + day totals | 8 ms | 6 MB |
| live update incl. weekly segments (≤ every 2 min) | ~340 ms | ~440 MB |

The cost of an update does not grow with the age of the record (the old live build allocated 2.4 GB per build after
one simulated week, growing every week).

## The record file

`usage-record.sqlite` next to `state_path` (owner-only, WAL). It holds:
- a copy of the forward ledger's requests and the observation store's meter readings, indexed by account and time
  (cursors per source store; a different store is copied from the start, rows are keyed by ID);
- the requests of providers Pacer records only for this page (below) — these exist only here;
- sealed segments; judged capacity intervals (for "Verified"); the last models.dev sync.

Removing the file rebuilds everything except the usage-only requests.

## Accounts and links

An atomic account is an imported CPAMP account or a Pacer native account reference. A native account certified (at
the cutover) to exactly one imported account is that account for every request and reading, whatever credential
carried it (an implicit link: a re-login keeps its history). The user can link any login to an earlier one of the
same provider ("This is a new login of …"): their evidence becomes one account, its windows one list. Links are
append-only revisions (`account-links` objects in the forward store); unlinking is a new revision. The page names
accounts and logins by opaque references (`acct:<sha256[:24]>`); imported IDs, which encode file names, never reach
the browser.

## Profiles

The record's profile configuration is the imported one with each profile replaced by its forward ledger revision
(same ID), plus the forward ledger's own profiles; bindings of replaced profiles stop applying. Usage page edits
(create, rename, archive/restore, add an account from its first use or from now, stop counting it, remove it with its
history) are saved as forward profile revisions with explicit memberships for every login of the account. A
binding-based profile is converted with its exact first-binding ownership, so its contents do not change. Only the
accounts whose intervals changed are derived again.

## Pools (`usage_pools.go`)

Every reading is mapped to one canonical pool before derivation; window IDs are kept.

| provider | pools |
|---|---|
| Anthropic | `all` (5h, weekly); `fable` (weekly 7d_oi: the Fable models; Max plans only) |
| Codex | `codex-main` (weekly; CPAMP's `all`, `codex_main` and header `codex-main` are one meter); Spark (ended) |
| Antigravity | `gemini` and `claude_gpt`, each 5h and weekly |
| OpenCode Go | `all`: 5h (rolling), weekly, monthly (calendar month from the reset) |

On real data the Fable pool gets a rate for the first time (Claude Max 20x: $6.00–8.27 per 1% over 6 windows) and
the duplicate empty Codex pool disappears without changing the measured Codex rate. A pool that stopped reporting
while the card's other pools kept reporting is shown under "Ended limits".

## Usage-only providers

Credentials of providers other than Claude and Codex (Antigravity, OpenCode Go, ...) are discovered separately. They
never enter routing, coverage, the measurement report or the forward ledger. Their final requests go straight to the
record file; their meters are read by the observation scheduler (at most every 15 minutes per account):
- Antigravity: `POST https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary` with the login's
  project, parsed like CPAMP's poll (Gemini group; Claude and GPT group; 5h and weekly buckets);
- OpenCode Go: `GET https://opencode.ai/zen/go/v1/usage` with the login's key (`rolling`, `weekly`, `monthly`).

Both endpoints are unofficial. They are verified with a fake host in tests; the production dry run is the first real
read.

## Prices

The price of a model: your price (Model Settings), else the imported book, else models.dev. models.dev is fetched once
a day and on request through CPA's HTTP client (so CPA's proxy settings apply); only the providers Pacer serves are
kept. Matching reproduces 32 of the 33 prices the import took from models.dev byte for byte (the other one changed in
the catalog since). OpenCode Go gets a reference unit (DeepSeek V4.1 Flash) so its $ per 1% can be shown. Your prices
are append-only revisions in the forward store.

## Verified

Pacer's capacity check (`capacity-lower-bound.md`) proves for an interval that every request on a Claude account went
through Pacer, which gives a floor for $ per 1%. Each judged interval is kept in the record file (a later judgment can
revoke one), and a window containing a proven interval shows "✓ Verified" with that floor.
