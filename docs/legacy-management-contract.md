# Frozen legacy management contract

This additive contract is for the native CPA management surface and the CPAMP
credential-free iframe bridge. Base: parent `129121f`; domain library:
`internal/historyreports` from `79b9e83`. No route imports or prepares an artifact.

## Exact allowlist (bridge integration)

Root: **`/v0/management/plugins/pacer/history/legacy/`**. Register these exact
method/path pairs; no prefix proxy, resource alias, redirects or arbitrary URL.

| Method | Suffix | Request body | Response |
| --- | --- | --- | --- |
| GET | `status` | None; no query parameters | `historical_ready`, `default_pin`, `coverage` |
| POST | `profiles` | `{ "query": LegacyQuery }` | `LegacyProfileReport` |
| POST | `profile-proof` | `{ "query": LegacyQuery, "window_id": string }` | `LegacyProfileProof` |
| POST | `history` | `{ "query": LegacyQuery, "kind"?: string }` | `LegacyHistoryReport` |
| POST | `history-proof` | `{ "query": LegacyQuery, "window_id": string }` | `LegacyHistoryProof` |
| POST | `forecasts` | `{ "query": LegacyQuery, "window_id": string, "limit"?: integer, "cursor"?: string }` | `ForecastPage` |
| POST | `quota` | `{ "query": QuotaQuery }` | `QuotaReport` |
| POST | `export` | `{ "query": LegacyQuery, "view": string, "window_id"?: string, "kind"?: string, "format": string }` | Download envelope |
| POST | `quota-export` | `{ "query": QuotaQuery, "format": string }` | Download envelope |

POSTs accept no URL query parameters. Omitted `query` means defaults; `{}` is
valid for lists. Proofs/forecasts require a nonempty window ID. An operation must
reject fields belonging to another operation **even when zero/empty**. JSON keys
are case-sensitive; duplicate keys, aliases, unknown nested fields, null objects,
null pins/scalars, non-object bodies and trailing JSON are rejected. Optional
slices may be null, matching the existing strict report scanner.

The bridge should forward request/response objects without reshaping report DTOs,
pins or numeric fields. Keep management credentials entirely in the host's
existing authenticated session; the iframe gets no key, token or artifact path.
The direct native page holds its CPA management key only in page memory.

### Query types

`LegacyQuery` is either `{ "selection": Query }` or `{ "pin": LegacyPin }`.
Native `Query` supports `profile_reference`, `account_reference`, `model`,
`key_hashes` (at most 100 lowercase SHA-256 fingerprints), `from`, `to`. Embedded
workspace pins/evidence pins/request pagination are not accepted in a legacy
selection. Do not combine a pin and selection overrides.

`LegacyPin` is the source `ProfileFilter`, with `at`, `through`,
`observations_through`, `forecasts_through`, `verifications_through`, `revision`,
`estimator`, optional `valuation_rule`/`token_evidence_through`, and source selection
fields `profile`, `account`, `model`, `api_key`, `from`, `to`. Use a complete pin
from status or a returned report. An explicit old pin with omitted estimator
retains v2 semantics. Workspace `page` is not a legacy pin field.

`QuotaQuery` is either `{ "pin": EvidencePin }` (complete source QuotaUsageFilter)
or `{ "selection": Query, "kind"?, "period"?, "window"?, "basis"?,
"source_plan"?, "target_plan"?, "capacity_ratio"?, "gateway_only"? }`.
Defaults: current period at frozen H, today basis = pinned source catalog.
Kinds: empty/5h/weekly; periods: current/history; basis: today/then. A finite
positive ratio up to 10,000 needs one window and a valid target tier. Explicit
source plan also needs one window. Preserve source reason/status and uncertainty.

`history` accepts only account/key/date selection and kind (max 64 bytes), not
profile/model. `history-proof` accepts only window/key selection and pin bounds,
not account/profile/model/date selection. `forecasts` is the full source-window
ledger; account/profile/model/key/date filters are rejected. It accepts 1–100 rows
(default 25) and a signed cursor (max 4096 bytes). Cursor binds the artifact,
complete pin, window and page size. Window IDs are at most 256 bytes.

### Exports

`export.view`: `profiles`, `profile-proof`, `history`, `history-proof`. Only
history list accepts `kind`; proofs require `window_id`; lists reject it.
Format is exactly `json` or `csv`. Download response is JSON:

```json
{"filename":"pacer-legacy-profiles.json","media_type":"application/json","content":"<exact domain writer bytes>"}
```

Filename is `pacer-legacy-<view>.<format>`; quota filename is
`pacer-legacy-quota.<format>`. CSV media type is `text/csv`. Do not reserialize
`content` or turn JSON exports into a second object. This uses the same envelope
as the existing history export bridge. Saved forecasts are present in profile
proof exports; there is no invented standalone original forecast CSV writer.
Original raw-history exports do not contain a report pin. The UI exposes a
separate exact loaded `{view,pin,kind}` record for saving/replay; copy that record
to retain kind selection as well as the frozen ledger bounds.

## Authentication, bounds and errors

Routes are advertised only as CPA management routes (`/plugins/pacer/history/...`
in the native plugin manifest), never public resource routes. Existing host CPA
management authorization applies before dispatch. No HTTP capability accepts
filesystem paths, SQLite, import/preparation, profile mutations or credentials.

All report routes share the existing **two-slot nonblocking gate**, **64 KiB**
input and **8 MiB** encoded-response/export limit. The JSON envelope itself also
must fit. Filtered cold calculations retain cooperative 30-second context
cancellation, not a hard CPU deadline. No timing-based admission test is needed.

Errors remain sanitized `{"error":"..."}` without source errors, SQL or paths:
400 invalid input/bounds; 404 unknown exact method/path; 409 missing artifact,
historical preparation required or mismatched paging cursor; 413 size limit;
429 two occupied report slots (`Retry-After: 1`); 503 unavailable evidence;
504 cooperative deadline. GET status with an older artifact returns 200 and
`historical_ready:false`; new read/export methods return 409 until offline
re-preparation. Existing Workspace methods continue working.

## UI truth and pending acceptance

Advanced **Legacy & Scenarios** sits under Usage Quota, with structured legacy
profile totals/windows, exhaustion history, saved forecast/revision rows and
subscription scenario comparisons. Evidence JSON is supplemental. Labels must
distinguish captured cost, observed provider percentages, estimator bounds,
explicit scenario/gateway-only assumptions and unavailable/missing evidence.

H, ledger cursors, estimator and exact pins stay visible. Dates are UTC and
half-open. Saved replay uses the pin alone. No refresh button claims to collect
provider data or issue/rescore predictions. All added state and downloads are
generation-guarded and cleared on disconnect; API text uses `textContent`.

Legacy CSV map-backed row order is deterministic natively, with exact source row
bytes; full-file parity with every original random map iteration is not claimed.
See [domain semantics/exclusions](history-reports.md).

## Source verification and exact remaining acceptance

Implemented: nine exact native management routes, shared admission/limits and
strict typed scanner, additive structured UI, source snapshot/pin replay and
exact domain export envelopes. Source tests compare **53 complete domain responses
and 104 domain-writer exports** through management dispatch. They also check
resource/mutation/preparation refusal, nested duplicate/case/null/unknown fields,
future pins, paging tampering, the shared full gate, exact input/output limits,
sanitized errors, missing artifacts and old-artifact readiness.

Focused race tests (`./internal/pacer ./internal/management`) and repository-wide
`go vet -tags sqlite_fts5 ./...` passed. The Node test syntax-checks the actual
inline script and executes it against frozen original DTOs with a small mocked
DOM. It checks structured rows, text-only API data, exact replay/scenario pins,
forecast revisions, proof downloads, old-artifact errors and deterministic
deferred-response/logout/download guards. No timing sleeps or live endpoints are
used. Set `PACER_NODE=/absolute/path/to/node` to run that source test if Node is
not on PATH:

```sh
go test -race -tags sqlite_fts5 -count=1 -timeout=240s ./internal/pacer ./internal/management
go vet -tags sqlite_fts5 ./...
```

**Real native browser and CPAMP bridge acceptance remain pending.** Parent must:

1. Prepare/load a new immutable artifact offline; confirm `status` reports ready.
   For meaningful forecast acceptance, use the historical extension's synthetic
   fixture (`historical-golden/historical-fixture.sql.gz`, event cursor 240,
   observation cursor 65, H `1790812800000`), or the retained original synthetic
   SQLite at `evidence/implementation/history/reports-legacy-oracle/final/`.
   Merely re-preparing the older empty/forecast-free lab does not supply saved
   forecast rows. This work has not imported or replaced the parent's lab.
2. Grant the exclusive native-browser window. Verify authenticated navigation,
   host rejection of inference keys, profile/proof/missing-price rows, exhaustion
   states, v2/v3 replay, two saved forecasts with verified/unscorable revisions,
   explicit scenario ratios/reasons, old-artifact error, and downloaded bytes.
3. Check real desktop/mobile table overflow, proof dialog behavior and actual
   logout/in-flight cancellation behavior. Mocked DOM checks do not establish
   browser layout or host authorization acceptance.
4. Integrate/test the separately owned CPAMP bridge using the exact allowlist and
   envelope above; verify CPAMP-unavailable native operation independently.

No provider refresh/manual quota update, mutable source-profile API, forecast
issuance/rescoring worker, adaptive exchange, live measurement integration,
production backup migration, push/release/runtime change or retirement approval
is part of this source handoff. Existing library methods are unchanged.
