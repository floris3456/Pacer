# Forward v8 native wiring

Source integration on `6829fd5f5501237f635214b70afe3e652463a4f8`. This connects the
standalone [HTTP adapter](forward-http-contract.md) and
[native publisher](forward-runtime.md) through actual `Plugin.Handle`. Earlier
statements in those slice documents that routes are not wired describe their
pre-integration commits. **This is source/fake-host verification, not a deployment
or current native-host/auth/browser/physical-coverage acceptance.**

## Registration and capability lifetime

1. The existing `plugin.register` / `plugin.reconfigure` configure path opens only
   explicitly prepared/configured stores, as before. After success, Handle returns
   schema **7** if either cache policy or the forward publisher is configured.
   `capabilities.cache_policy` still reflects only the actual cache-policy adapter.
   Forward does not enable cache policy, prepare a store, select a price book,
   enable routing, clear cooldowns or override billing/coverage authority.
   Standalone legacy configuration still returns schema **6**.
2. No forward HTTP capability exists yet. Only a later trusted
   `management.register` SDK request may install the scoped advertisement.
   `PluginManagementBasePath` must equal exactly
   **`/v8/management/plugins/pacer`**. Empty/missing/old-host/wrong-prefix values,
   a missing configured store or a stopped instance produce no forward routes.
   Schema, legacy BasePath, display metadata, client Body/Query/Path or inference
   headers never substitute for the advertisement.
3. Registration input is bounded to 64 KiB and decoded into the actual vendored
   SDK request after an exact-case typed scan. Unknown fields, duplicate keys at
   any nested level, null capability, trailing JSON and malformed shapes cannot
   grant forward authority. The existing Pacer metadata has no ConfigFields;
   nonempty extensions fail closed for forward. Native `host_callback_id` may be
   present but is not an authorization credential.
4. The existing legacy/resource registration JSON and path order remain unchanged;
   nine exact forward route declarations are appended only after successful
   capability validation. A malformed/empty registration keeps the historical
   registration behavior but **revokes** any old forward capability.
5. Each binding owns a revocable context and is tied to that configured publisher.
   Re-registration is serialized; previous bindings are canceled before parsing
   the next registration. Plugin register/reconfigure attempts revoke before and
   after configure (including failures), closing the concurrent parse/configure
   gap. A fresh management.register is required even after a same-config reload.
   Changed forward configuration still requires idle reload/new instance; nothing
   changes `configure` or its DDL/namespace initialization order in this slice.
6. Quiesce/shutdown explicitly revoke, cancel and join. Close cancels the binding
   through its parent publisher context, rejects new work via `p.stopped`, and
   joins active leases before closing the store. Closed/quiesced instances cannot
   reactivate on reconfigure or management re-registration.

CPA remains the management authentication authority. It must authenticate its
scoped v8 Gin route before the native `management.handle` call. This wiring does
not implement a public HTTP server or an alternative authentication scheme.
Prior CPA Gin tests cover inference-token rejection/401; fake-host plugin tests
here do **not** claim to prove HTTP authentication. Full matching Path alone is
insufficient: a live binding and a configured store lease are also mandatory.

## Dispatch, store lease and shutdown

`Plugin.Handle(management.handle, raw)` selects the strict forward branch before
the unchanged legacy decoder/handler. Detection recognizes any top-level v8 Path,
including duplicate or case-alias Path fields, so last-wins decoding cannot hide
an attempted v8 call behind a legacy Path. The adapter then validates the complete
native SDK request, verb, exact path, headers, query and typed body. Malformed or
unregistered aliases have no forward handler fallback.

`handleForwardManagement(ctx, raw)` is the internal context-aware entry point.
The current exported `Plugin.Handle` ABI has **no caller/HTTP IO context**, so it
passes Background; this limitation is explicit rather than inventing request
disconnect cancellation. The internal entry honors an actual caller context if
one becomes available, and every invocation always combines its 30-second deadline
with binding revocation and publisher shutdown cancellation.

The base has `WithForwardReports`, which combines a lifetime lease and admission.
The new `WithForwardStore(ctx, fn)` adds only the **lifetime lease**:

- Checks the configured/non-stopped owner, takes its `life` lock, adds to the same
  publisher join group before releasing the lock, and hooks the owner context.
- Calls `fn` with the combined context and store; fn must honor cancellation and
  must not retain/close the store or return a database handle.
- Drops the lease only after the callback, response validation and serialization
  bound checks finish. A stopped publisher cannot add a new borrower while Close
  is waiting. The existing runtime's wait-before-close order remains authoritative.
- Takes **no** admission slot. `NewForwardHTTP` is constructed **inside** the
  callback and calls `HandleRPC` before the lease ends. No adapter/store pointer
  is cached after the callback. The adapter takes exactly one nonblocking slot
  from **`p.reportSlots` (capacity two)**. Historical reads and publisher sealing
  use that same channel; no new pool, publisher or relay goroutine is introduced.
- No analysis, status SQL, serialization or wake runs under `p.mu`.

Busy requests return fixed 503/busy. Cancellation/revoked leases return sanitized
503; unadvertised bindings return 404. Reads cannot become a successful 200 after
observed cancellation. A profile CAS that has **already committed** remains
202/pending even if notification or cancellation races afterward. The runtime-owned
`w.wake` channel is passed under the lease; it is coalesced, nonblocking, remains
open for its entire lifetime, and is sent only after successful CAS. No head is
fabricated and no second notification worker is added. Durable profile revisions
and the existing periodic publisher recheck remain authority if a hint coalesces.

The optional unexported `forwardManagementLeaseHook` is a deterministic test
barrier, set only before starting an instance, never configured by production or
request input. It proves cancellation/join while a real Handle request owns a
lease without fault-injection DDL or arbitrary sleeps.

## Exact endpoints and request contracts

Every full path begins **`/v8/management/plugins/pacer/reports/forward/`**:

| Method | Suffix | Body / query |
| --- | --- | --- |
| GET | `status` | No body/query |
| POST | `pin` | `{generation?:string, filter?:ForwardFilter}`; latest captured once only here |
| POST | `report` | `{pin:ForwardPin}` |
| POST | `requests` | `{pin:ForwardPin,limit:1..100,cursor?:string}` |
| POST | `evidence` | `{pin:ForwardPin,id:string}` |
| POST | `export-json` | `{pin:ForwardPin}` |
| POST | `export-csv` | `{pin:ForwardPin,limit:1..100,cursor?:string}` |
| GET | `profiles` | Exactly one `?revision=<canonical int64 decimal>`; no body |
| PATCH | `profiles` | String `expected_revision` plus dated `profile` or `archive_id` |

All post-pin reads/exports require exact version, immutable generation and explicit
filter. Cursor binds generation/filter/**limit**. No implicit refresh per page or
alias under v0/resource/relative paths. See the adapter contract for the complete
typed allowlists: UTC RFC3339Nano time strings, exact int64 revision strings,
positive bounded millisecond membership-date strings, <=100 memberships and native
reference/provider/overlap rules. Books, origins, baselines, coverage and price
overrides remain offline-authority-only. Library calculation/source formulas and
the parallel profile/baseline/feed hardening are not changed here.

### Status: ledger capture versus publisher

The existing adapter's `state`, `feed_id`, quoted `projected_through` /
`bootstrap_through`, `latest_generation` and `runtime` are retained. A new typed
`publisher` object includes runtime state/phase, active flag, quoted source head /
projected / failures, bootstrap_complete, latest_generation, report_at, target_at,
fixed error_code and capacity `unavailable`. State, phase, errors, heads, capacity
and generation digest are validated against closed allowlists/bounds before
serialization. No raw SQL/error string/map is accepted as status.

These are explicitly **separate, non-atomic snapshots**: projected sequence is not
a published watermark, request count or complete-coverage claim. Only the publisher's
last sealed generation/report_at describe its last publication. Initializing maps
to adapter runtime unready, lag to lagging; the exact state remains in publisher.
Errors retain the prior sealed generation without being relabeled ready. A missing
store is disabled internally and has no forward HTTP capability. Quiesce/Close
leave internal runtime stopped and refuse stale HTTP work with 503. Failure to
obtain a lease/status returns an error, never a fabricated empty/ready report.

### Output and native envelope bounds

The adapter retains 64 KiB HTTP body, 4 KiB encoded query and 128 KiB encoded native
request limits; strict duplicate/unknown/case/trailing/null/type/path/header rules
and safe no-store/nosniff response headers remain unchanged. No pagination header
is added to the host allowlist.

Exports remain JSON `{filename,media_type,encoding:"base64",content,next_cursor?}`,
with fixed `pacer-forward.json` / `pacer-forward.csv` filenames. Decoding content
produces exact native writer bytes; CSV is one page including its original header.
Continue with the same explicit pin/limit and returned cursor. To reduce a limit,
restart at the empty cursor at the same pin. No download UI is included.

In addition to the adapter's **8 MiB full HTTP JSON body** cap, the wiring bounds
the complete **8 MiB native `pluginabi.Envelope` JSON serialization**, including
SDK response headers, Body base64 and the already-base64 export content. This is
intentionally stricter than the body-only cap. A roughly 4.5 MiB raw download may
exceed that double-encoded envelope once metadata is included. Oversized output
returns a small sanitized 413, never truncated 200; no host SDK/C ABI/protocol or
header allowlist change is needed. Error/busy replies pass the same SDK bytes path.

## Verification and integration boundary

Source tests use real prepared local stores, fake-host SDK registration, actual
Plugin.Handle usage and async publisher, exact $7.25 original library value,
unknown capacity, profile PATCH publication hints, pinned export replay and
C ABI byte transport. Deterministic barriers cover shared history/publisher slots,
Close/quiesce/re-registration/reconfigure cancellation and join, plus strict RPC
refusals and both levels of envelope base64. No fault-injection DDL is installed
by the new tests, preserving compatibility with the parallel feed schema fence.

The vendored SDK and all provenance manifests are byte-unchanged. Existing
configure namespace/DDL initialization, runtime workers, historical/native
libraries, measurement/feed and all v0 handler bodies remain untouched. Parent
must integrate the separately owned hardening, run combined configure/source
checks, then authorize/build/load consistent host/plugin versions for deployed
native acceptance. No source result clears startup faults, establishes current
physical coverage, enables retirement or authorizes any lab call.
