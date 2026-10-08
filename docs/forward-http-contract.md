# Standalone forward reporting HTTP adapter

Source slice based on Pacer `9c38a70`, consuming CPA v8 host support `9af81ead`.
**Not wired or advertised by the running plugin.** This adds no lifecycle,
publisher, configuration, browser, measurement or calculation implementation.
The forward-design draft's v0 proposal is superseded by this v8-only contract.

## Parent integration surface

All implementation is in new `internal/pacer/forward_http.go`:

```go
routes := forwardV8Routes(hostreg, enabled) // pure []map[string]string declarations
adapter := NewForwardHTTP(store, p.reportSlots, ForwardHTTPOptions{
    Registration: hostreg,
    Enabled: enabled,
    Status: runtimeStatus, // optional func(context.Context) (ForwardHTTPRuntimeStatus, error)
    Notify: publishWake,   // optional caller-owned chan<- struct{}
})
response, err := adapter.HandleRPC(ctx, pluginabi.MethodManagementHandle, raw)
// Or, after the parent's native SDK envelope decode:
response, err = adapter.HandleManagement(ctx, sdkRequest)
```

`store` is `*historyreports.ForwardStore`, owned/opened/closed by the runtime
owner. `p.reportSlots` must be the **same buffered two-slot channel** used by
existing historical requests. Nil, unbuffered, one-slot or >2-slot channels fail
closed with 503; this adapter allocates no admission channel. Caller must keep
the store and admission channel alive until in-flight calls drain and must not
close the admission channel. The adapter never closes either resource.

Parent must append declarations during its existing `management.register` path
and delegate only explicit forward paths from `management.handle`, using the
same advertisement/configuration snapshot for declarations and handler. Replace
the adapter on capability/configuration changes; do not mutate it concurrently.
No existing `Plugin` fields, registration or dispatch switch is changed here.
The constructor returns an immutable binding, not a publisher or registration.

### Host capability and authentication

The sole capability is **nonempty exact**
`hostreg.PluginManagementBasePath == "/v8/management/plugins/pacer"` plus explicit
`Enabled`. Wrong owner, trailing slash, empty/old host, false enabled, relative
paths or legacy BasePath never enable routes. Schema version alone is insufficient.
An unadvertised/disabled adapter refuses all requests with 404, even if a direct
caller supplies an inference/private-capability header.

**Parent prerequisite:** at this base, `registration(policyEnabled...)` advertises
schema 7 only for enabled cache policy, otherwise 6. CPA advertises the scoped v8
prefix only to schema >=7 registrations. Parent must decide its schema-7
registration condition when wiring forward mode independently of cache policy;
this slice does not alter `plugin.go`. The retained vendored pluginabi constant
also remains historically 6; copying types.go does not change runtime registration.

The trusted CPA scoped v8 host must already enforce management authentication.
This adapter does not authenticate headers or expose a public server. CPA's real
Gin auth/owner tests belong to the independently accepted host change; standalone
adapter tests are not new deployed HTTP/auth acceptance. No v0/resource aliases,
auth-URL fallback, imports, books, origins, baseline writes, SQL, authority/coverage
overrides, routing changes or cooldown clears are installed.

## Exact endpoints

Prefix: **`/v8/management/plugins/pacer/reports/forward/`**. All methods and paths
are literal; suffixes, case aliases, percent escapes, dots, doubled/trailing
slashes, embedded query/hash syntax and unsupported verbs return 404.

| Method / suffix | Exact input |
| --- | --- |
| GET `status` | Empty body, no query |
| POST `pin` | `{generation?: string, filter?: ForwardFilter}`; omitted/empty generation captures latest **once** |
| POST `report` | `{pin: ForwardPin}` |
| POST `requests` | `{pin: ForwardPin, limit: integer 1..100, cursor?: string}` |
| POST `evidence` | `{pin: ForwardPin, id: string}` |
| POST `export-json` | `{pin: ForwardPin}` |
| POST `export-csv` | `{pin: ForwardPin, limit: integer 1..100, cursor?: string}` |
| GET `profiles` | Exactly one `revision=<nonnegative canonical int64 decimal>` query value; no body |
| PATCH `profiles` | `{expected_revision: decimal-string, profile: ProfileEdit}` **or** `{expected_revision: decimal-string, archive_id: string}` |

POST/PATCH routes permit no query, including fields belonging to another operation.
GET requests permit no body, including whitespace. Missing required pin/limit/id/
revision fields are not defaulted. No latest selection occurs on report, evidence,
page or export requests. Unknown/missing generations are not replaced by latest.

`ForwardPin` is `{version:"pacer-forward-ledger-v1", generation:<64 lowercase hex>,
filter:{domain:"combined"|"native"|"baseline", ...}}`. Filter is mandatory in a
saved pin, as is its explicit domain. Pin creation alone defaults omitted domain
to combined. Optional filter fields: `from`, `to`, `profile`, `account`, `provider`,
`model`, `key_hash`. Times are canonical positive UTC RFC3339Nano **strings** in
the signed Unix-nanosecond range; no numeric timestamps or millisecond rounding.
From is inclusive, to exclusive, subject to the library's immutable report horizon.
Key hashes must be 64 lowercase hex; account <=1024 bytes; profile/provider/model
<=256 bytes; cursor <=4096 bytes; evidence ID <=2048 bytes. Text is control-free.

The pin names an immutable generation and canonical filter; it is **not a signed
authorization token**. A different valid filter defines a different selection.
Library cursors bind the exact pin/filter **and page limit**; cross-generation,
cross-filter, cross-limit or altered cursors return 409. No top-level filter or
other overrides can accompany a saved pin. Calculations, source validation,
valuation, coverage reasons and cursor verification remain in the library.

### Profile edits and publication

`ProfileEdit` permits only `id`, `name`, `provider`, optional `selection`, and a
required `memberships` array (0..100). Each membership contains `account`,
`start_ms` and optional `end_ms`. Names/IDs/providers <=256 bytes, account <=1024.
Dates are **canonical positive decimal strings** representing integer UTC
milliseconds; supplied end must exceed start. Omitted end is open. Numeric/float,
sub-millisecond, negative/zero, overflow and out-of-range dates are rejected.
Library validation owns known account/provider references, same-account overlap,
reserved profile IDs and provider immutability. Creation/archive metadata on an
existing profile is preserved; memberships become the explicit dated selection.
No key metadata or provider authorization is changed.

`expected_revision` is an int64 **JSON string**, never a JSON number. Values beyond
2^53 remain exact, not rounded. Failed CAS returns 409; profile reads use one exact
revision, not current/latest. GET quotes both the branch revision and the retained
configuration revision while retaining original profiles, bindings and plans.

Successful mutation returns **202**:
`{revision:"N", publication:"pending", notification_enqueued:true|false}`.
Only after successful library CAS does a nonblocking send target the caller's
notification channel. Full/nil/closed notification channels cannot undo a committed
write or produce a fake failed mutation. There is no publication goroutine here;
durable revision state remains authority. Cancellation after a successful commit
also does not rewrite success as failure. A future publisher must seal a new
generation; existing pins remain unchanged. Clients reconcile an uncertain network
result by reading the revision rather than blindly retrying a new mutation.

## Status and conservative scope

Store-only status returns version, state `unready` or `snapshot_available`,
`feed_id`, quoted `projected_through` / `bootstrap_through`, and
`latest_generation` (possibly empty). Projection through is not a published
watermark or coverage claim. Without a runtime callback, `runtime.state` is
**unknown**, not ready. The callback supplies only a validated state from
`unknown/unready/ready/lagging/error/disabled` and optional quoted nonnegative
`target_through`. Arbitrary state strings/errors are rejected/sanitized.
Unready/error/lagging states are preserved rather than upgraded. Store failures
return 503; no latest pin is fabricated. No current physical coverage is inferred
from a historical sealed generation or this adapter.

## Transport, budgets and downloads

- Actual SDK `ManagementRequest` / `ManagementResponse` are used. `HandleRPC`
  accepts only `management.handle`; native JSON uses PascalCase `Method`, `Path`,
  `Headers`, `Query`, base64 `Body`, and optional `host_callback_id`. It never uses
  callback ID or headers as credentials. Unknown/duplicate/case-alias native
  envelope fields are rejected, with a bounded 128 KiB encoded RPC input.
- HTTP body limit **64 KiB**, query limit **4 KiB**; native direct handling applies
  both. Strict JSON scans every nested object for duplicates and exact field names;
  refuses trailing data, scalar/object nulls, invalid UTF-8, unknown fields and
  wrong types. Bound depth 12, nodes 4096, object fields 64, arrays 128 (profiles
  further bound memberships to 100), strings 4096 except the encoded RPC body.
  SDK nil header/query/body containers are accepted only in the outer RPC envelope.
- Input headers may only be Content-Type (application/json) and Accept, each one
  control-free <=4096-byte value. If-Match is not an alternate profile CAS input.
  Auth, inference keys, cookies and private capability headers are refused, never
  forwarded to the store or echoed. Nil headers are valid after trusted dispatch.
- Shared admission is immediate/nonblocking: full returns **503 busy**. The
  historical **30-second cooperative deadline** derives from caller context and
  is never extended. The slot stays held until work actually returns; no hard
  timeout, runaway goroutine, independent capacity or third publisher is created.
- Full response JSON, including download metadata, base64 expansion, continuation
  and newline, must fit **8 MiB**. Raw writer overflow, short writes or any export
  error produce a bounded failure, never successful truncated output. An export
  that fits the library's raw limit may need a smaller page to fit this envelope.
- JSON/CSV exports return `{filename,media_type,encoding:"base64",content,
  next_cursor?}`. Fixed filenames `pacer-forward.json` / `pacer-forward.csv` and
  media types application/json / text/csv; decoded content is the **exact native
  writer bytes**, including newline/CSV header. JSON export is a pinned summary;
  CSV export is one request page. Continue CSV with the returned cursor, same pin
  and same limit. To lower a page limit, restart from the empty cursor at the same
  pin; an old cursor cannot be reused with a different limit. Every part retains
  its native header. No pagination headers,
  URLs or weakened host allowlist are required. A future browser constructs the
  Blob from decoded content; this task adds no download UI.
- Success and error headers: application/json, Cache-Control no-store,
  X-Content-Type-Options nosniff. No arbitrary response headers, redirects, cookies
  or private bindings. CPA may replace/retain these with its own safe host headers.
- Fixed sanitized error categories: 400 invalid request; 404 route/reference;
  409 cursor/reference conflict; 413 input/output bounds; 503 busy/unavailable/
  pending/cancel/deadline. Raw SQL, filesystem paths, credentials and request text
  never appear in adapter errors. Native evidence/domain DTOs remain library-owned.

## SDK provenance and verification scope

Only `third_party/cpa-sdk/sdk/pluginapi/types.go` is refreshed, byte-for-byte from
CPA `9af81eadc59475eba69de16f586b4fb555791515`, SHA-256
`1c72f5459f04d9d9f81c25686999e7022b7d9935731259f4cb2e9ada2d5ac2de`.
`V8-PROVENANCE.json` records the replacement and the preserved historical manifest
hashes. The vendor README now explicitly describes the mixed snapshot. The
upstream file also includes existing Path/reset type declarations; no reset,
active-mode implementation or call is introduced. Original manifests are intact.

Tests use a real local ForwardStore fed by the existing native converter/feed,
native library valuation and pinned JSON/CSV writers, plus real SDK/C ABI JSON
encoding. They do not fake a public HTTP API or modify source formulas/oracles.
Current host/runtime registration, deployed native positive acceptance, browser
downloads, current physical proof, forward live coverage and retirement remain
unverified until parent wiring, builds/reload and explicit acceptance.
