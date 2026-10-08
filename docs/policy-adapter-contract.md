# CPA → durable Pacer policy adapter

`internal/policyadapter` bridges CPA native **schema 7**, client protocol **1**, policy contract
revision **2** to `internal/cachepolicy`. It implements `pluginapi.CachePolicyProvider` and
`pluginapi.RequestLifecyclePlugin`. This package does not register the parent plugin, authenticate
HTTP requests, dispatch upstream requests, poll providers, or create a database on request failure.

Authoritative CPA source inspected: commits `6469ec4`, `bb0c668`, `d314c85`,
`sdk/pluginapi/cache_policy.go`, `docs/pacer-contract.md`, and the native
`rpcRequestCompletion` wrapper in `internal/pluginhost/rpc_schema.go` / `rpc_client.go`.
The durable state machine remains specified in [cache-policy-contract.md](cache-policy-contract.md).

## Construction and first installation

Exact exported configuration/API:

```go
type Route struct {
    ProviderRoute string
    Models []string
    TTLs []cachepolicy.TTL
}
type EmptyClaim struct {
    Scope pluginapi.CachePolicyScope
    ContextEpoch string
    OperationID string
}
type NewEmptyAuthority func(context.Context, EmptyClaim) (bool, error)
type Options struct {
    Clock func() time.Time
    Routes []Route
    NewEmptyAuthority NewEmptyAuthority
}

PolicyOptions(Options) (cachepolicy.Options, error)
Open(context.Context, *sql.DB, Options) (*Adapter, error)
(*Adapter).Dispatch(context.Context, string, []byte) (any, error)
(*Adapter).HandleCachePolicy(context.Context, pluginapi.CachePolicyRequest) (pluginapi.CachePolicyResponse, error)
(*Adapter).ResolveCachePolicy(context.Context, pluginapi.CachePolicyResolveRequest) (pluginapi.CachePolicy, error)
(*Adapter).CachePolicyAttempt(context.Context, pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error)
(*Adapter).HandleRequestComplete(context.Context, pluginapi.RequestCompletion) error
(*Adapter).CompleteRequest(context.Context, pluginapi.RequestCompletion) (bool /* process-owned or already completed */, error)
```

Example normal startup:

```go
opts := policyadapter.Options{
    Clock: time.Now,
    Routes: []policyadapter.Route{{
        ProviderRoute: "claude-main",
        Models: []string{"claude-opus-4-6", "claude-sonnet-4-6"},
        TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h},
    }},
    NewEmptyAuthority: nil, // default: no trusted empty-session attestation exists
}
adapter, err := policyadapter.Open(ctx, db, opts)
if err != nil {
    return err // managed policy remains unavailable; no unmanaged/default-TTL fallback
}
```

The parent owns/closes `db`. Configure SQLite on every connection using a durable DSN such as
`?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000`; do not rely on routing-store defaults.
`Open` only calls `cachepolicy.Open`: absent/corrupt/unsupported correctness state fails closed.
It never falls back to `Create`, including during registration, reconnect or an inference request.
An **explicit first-install tool/controller**, outside all request/startup-error paths, can call
`PolicyOptions(opts)` and then `cachepolicy.Create(ctx, db, policyOptions)` once. Route mappings and
TTL lists are copied; they cannot be changed by mutating the input slices after construction.

Each route needs an exact, externally configured allowlist of **host-resolved built-in Claude
model IDs**, supported TTLs, and a unique route ID. There are no implicit routes or model defaults,
wildcards, alias resolution, case folding or provider rewrites. The parent must source these IDs
from verified built-in Claude configuration; a name prefix is not proof of executor provenance.
CPA additionally owns the actual provider/transport check. Empty routes disable selection.

## Parent registration/dispatch integration

Register plugin ID `pacer` with `schema_version:7`, `capabilities.cache_policy:true`,
`request_lifecycle_plugin:true`, and `usage_plugin:true`. Keep policy handling active independently
of quota routing mode. Do not advertise working selection on schema 6 or when `Open` failed.

The parent's existing dispatcher should forward:

```go
switch method {
case pluginabi.MethodCachePolicyHandle,
     pluginabi.MethodCachePolicyResolve,
     pluginabi.MethodCachePolicyAttempt:
    return adapter.Dispatch(ctx, method, raw)
case pluginabi.MethodRequestComplete:
    result, err := adapter.Dispatch(ctx, method, raw)
    if err != nil { return nil, err }
    // Then perform the parent's existing logical tracker completion/cleanup.
    return result, nil
}
```

`Dispatch` handles exactly these four RPCs; recovery is not an RPC. The parent remains responsible
for its other methods and usage observer. With no adapter, the cache-policy RPCs must return a
503 rather than bypassing policy. The shared completion callback still runs for unmanaged requests;
unknown completion IDs are no-ops and never release unowned durable guards.

**Trust boundary:** call `Dispatch` only with native payloads constructed by the authenticated CPA
host. Do not expose it as a public HTTP handler or forward client-chosen `scope.caller_scope` into
it. HTTP origin/authentication, private header handling, exact method/path allowlist, and CPA's
transport/executor capability gate remain host responsibilities.

The adapter uses the actual additive vendored SDK structs in
`third_party/cpa-sdk/sdk/pluginapi/cache_policy.go`; no v8 imports or runtime dependency were added.
The usage additions preserve PascalCase outer `UsageRecord`/`UsageDetail` fields, optional split
counters, token-accounting version, frozen cache evidence and response-header observation clocks.
Three method constants plus `SchemaVersionCachePolicy=7` were added to `sdk/pluginabi`.
The parent plugin's existing registration implementation is not changed by this package.

### Exact RPC wire behavior

* `cache_policy.handle`: `CachePolicyRequest` → `CachePolicyResponse{status_code,body}`. `body` is
  `json.RawMessage` containing the durable State directly, **not base64**. GET requires operation
  `read`, protocol 1 and matching scope fields only; it never mutates. POST preserves all rev2 CAS,
  enrollment, receipt and optional timestamp fields. Operation scope must equal host scope.
  Domain errors return explicit HTTP status and `{"error":{"code":"pacer_cache_...","message":"..."}}`.
* `cache_policy.resolve`: `CachePolicyResolveRequest` → frozen `CachePolicy`. Both `claude` and
  `openai-response`, streaming/non-streaming, are supported for configured models and kinds
  `primary|warming`. Auxiliary summary/title/helper calls are not managed by this boundary; CPA
  sends them separately without a primary policy. There is no client TTL field. The adapter gets
  the active TTL, then Store atomically checks the supplied epoch/revision when beginning a request.
* `cache_policy.attempt`: `CachePolicyAttemptRequest`. `begin` returns
  `CachePolicyAttemptResponse{policy,started_at}`. Native `validate` and `finish` return `{}`.
  Direct typed methods return the SDK's zero response on those operations. Attempts use fresh
  physical UUIDs, exact logical ID, capability, frozen policy and (after begin) durable start.
* `request.complete`: public `RequestCompletion` fields are **PascalCase**, unlike new cache-policy
  structs' snake_case. Actual CPA native payloads can also contain top-level optional
  `host_callback_id`. It is accepted and ignored for ownership; arbitrary callback/metadata fields
  are never interpreted as caller identity or capability. Native success returns `{}`.

Native payloads are bounded to `MaxRPCBytes = 65536`; POST operation objects are additionally
bounded to 16384 bytes and require all common command fields. Duplicate/case-aliased/unknown fields,
null typed scalar/struct fields, invalid types and trailing JSON are rejected. Completion Metadata
maps remain extensible. Rejections never echo JSON contents or private error details.
RPC failures are `*pluginabi.Error`, including a safe `HTTPStatus` / `StatusCode()` for CPA.

## Caller/capability binding and the intended enrollment trust hook

`scope.caller_scope` is already CPA's lowercase 64-hex digest from
`SHA256("cli-proxy-api:caller-scope:v1\x00" + authenticatedPrincipal)`. It becomes
`cachepolicy.Credentials.CallerDigest` **unchanged**, never hashed again. The canonical 32-byte
unpadded base64url capability is supplied separately in native request/attempt fields from CPA's
protected closure. Missing capability fails 403; malformed encoding fails 400. Body scope and host
scope must match; session IDs alone grant no ownership.

**CPA currently provides no trusted new-empty attestation.** With the default nil authority,
`enrollment:"new_empty"` returns 422 `pacer_cache_enrollment_unsupported`. The label, missing policy,
new session ID, ordinary inference-key authentication and generation changes do not set
`Evidence.NewEmpty:true`.

The injectable `NewEmptyAuthority(ctx, EmptyClaim)` is the intended future hook for the parent's
separately established, authenticated trusted OCP-plugin empty-session attestation. It receives no
private capability and must check that separate authority. A hook that simply returns true when
the label is present would defeat the gate. Hook errors are redacted to 503
`pacer_cache_attestation_unavailable`; false leaves enrollment unsupported. CPA need not query the
private client DB. Already recorded successful operation replay still uses Store's durable receipt.

Existing/unknown/copied history works today through authenticated `prepare` with
`enrollment:"existing_history"`, revision 0, followed by new Core compaction and a transition-bound
receipt commit. No empty-history authority is needed for this flow. Until commit, active TTL is
unknown, primary/warming are refused and cancel does not make the scope fresh. All receipt rules,
clock-skew-independent provenance, operation-ID replay and CAS are the rev2 Store contract.

## Physical attempts, completion and cancellation

The adapter keeps an unexported logical owner keyed by CPA's host-generated global lifecycle UUID:
private Credentials, immutable resolved selection/model/format/stream, and physical attempt states.
Its mutex serializes process-local ownership; SQLite still supplies durable cross-call CAS and guards.
Capabilities are cleared from the retained resolve DTO and never enter snapshots, responses or usage.

1. A successful resolve durably begins the logical guard before returning. Identical in-process
   resolve retries recover the frozen response; changed scope/capability/model/reference/transport
   with the same ID is rejected. Already completed IDs cannot reopen.
2. Begin compares the entire supplied policy with that frozen selection and durably begins the
   physical attempt. Begin must have zero `started_at`, empty outcome, `dispatched:false`. A physical
   ID cannot begin twice—even for a lost begin reply. Treat uncertain begin acknowledgement as an
   outstanding guard and reconcile; do not redispatch that ID. Every actual retry has a fresh ID.
3. Validate requires the same policy/IDs/exact start, no outcome and `dispatched:false`, then calls
   Store.ValidateAttempt against the durable receipt. An already-created attempt can validate under
   its old TTL during prepare; new primary/warming attempts and retries pause. Closing/canceled
   logical work cannot newly dispatch. CPA still validates actual final body/headers separately.
4. Finish runs **only after transport/body shutdown**, checks the complete snapshot against the
   durable attempt and calls Store.FinishAttempt. A dispatched attempt must previously have passed
   final validation; `succeeded` additionally requires `dispatched:true`. Pre-dispatch failure or
   cancellation can settle without validation. Outcomes are precisely `succeeded|failed|canceled`.
   Duplicate same outcome/dispatch fact is idempotent while the logical owner remains; conflicting
   terminal evidence is refused. A canceled physical attempt blocks further retries on that logical ID.
5. Completion uses the retained private credentials, never mutable Metadata. `rejected` maps to
   Store `failed`; other three outcomes map identically. Success requires a settled successful
   dispatched physical attempt. Store refuses logical settlement while any physical attempt remains
   active. Completion intent blocks new retries; on error the owner/credentials remain for retry or
   recovery. The parent must surface settlement failures, not silently discard them.
6. Only after durable logical settlement does the adapter remove the private owner/capability. A
   non-secret outcome tombstone provides same-process duplicate-completion detection. Duplicate
   physical finish after logical completion is refused as unowned; the host's ordering contract
   requires synchronous physical settlement before logical completion. Tombstones have process
   lifetime; no receipt/tombstone pruning policy is implemented.

Settlement uses `context.WithoutCancel(ctx)` so a canceled client context cannot suppress a trusted
transport-stopped callback. This does not infer that transport stopped. Physical usage is separate,
asynchronous evidence and never settles guards; `usage.handle` is not an adapter dispatch method.

## Restart and explicit stopped-dispatch recovery

`Open` never expires/adopts existing guards. Resolve inventories durable outstanding work for its
authorized scope. Any request/attempt without a current process owner returns 409
`pacer_cache_recovery_required`, including a retry of the same logical ID after restart. Original
pending policy, active TTL and snapshots remain durable. An unknown completion after restart is a
no-op, **not** successful settlement; the next admission/commit remains blocked by the durable guard.

Only a **trusted host-local recovery controller** may use these additional methods:

```go
(*Adapter).Outstanding(ctx context.Context, scope pluginapi.CachePolicyScope, capability string) ([]cachepolicy.Snapshot, error)
(*Adapter).FinishStoppedRequest(ctx context.Context, scope pluginapi.CachePolicyScope, capability, requestID string, outcome cachepolicy.Outcome) error
```

`Outstanding` is read-only and scoped/capability-authorized. Before `FinishStoppedRequest`, the
controller must independently establish every physical dispatch for that logical ID has stopped.
Recovery accepts only `Failed` or `Canceled`, settles remaining physical guards first, then the
logical guard. It never invents success, compaction or a TTL. Already settled physical facts remain
unchanged. A storage failure may leave partially settled physical guards, but the logical guard is
retained; retry after repairing storage and verifying stopped dispatch. Both methods require the
retained/recovered private capability. Neither is exposed as an HTTP/native RPC operation.

Use a single owning adapter per host/store scope. Simultaneous independent processes cannot adopt
one another's active lifecycle IDs; they fail closed rather than coordinating distributed leases.
Missing/corrupt state and uncertain outcomes pause managed traffic. There is no automatic expiry,
reservation service, adaptive quota enforcement or provider call in this adapter.

## Verification boundaries

Tests use real SQLite, exact vendored CPA DTO JSON and an injected atomic clock. They cover control
CAS/attestation, both supported formats and stream modes, frozen attempts, foreign scopes/capabilities,
model/route refusal, retries/cancellation, native callback envelopes, outstanding recovery, I/O
failure rollback, concurrent attempts, missing state, clock-skew receipt forwarding and usage shape.
They do not claim an activated parent dispatcher, real CPA process or provider end-to-end acceptance.
