# Independent clients

Pacer owns account authorization, quota observations and durable cache policy.
A framework-independent TypeScript package, `@pacer/client`, owns bounded API
transport and wire validation. Each harness supplies its own small bridge for
credentials, session identity, durable history, cancellation and presentation.
The library lives in `opencode-pacer/packages/client`; its standalone package has
no OpenCode, Pi or UI dependency. It is local source, not a published npm release.

## Discovery and compatibility

`GET /v0/resource/plugins/pacer/events` keeps the existing protocol-1 fields and
adds `api`. Authenticated discovery uses the same endpoint with
`?view=capabilities&auth=api-key&v=1`, and returns the **API object itself**.
Use an ordinary CPA API key, never a management key or provider OAuth token.

[client-api.schema.json](client-api.schema.json) describes that object. Its
version is separate from the native plugin ABI, host schema and cache contract
revision. Ignore unknown additive fields. Unknown versions, disabled features
and absent host guarantees must not be treated as support.

| Field | Meaning |
| --- | --- |
| `build` | Plugin version, source revision and dirty status (`null` if unknown). |
| `host_schema` | Host registration schema; not proof of executor behavior. |
| `features.*.supported` | This host/plugin combination implements the operation. |
| `features.*.enabled` | The operation is configured; session authorization is still required. |
| `access` | Shared/granted account mode and optional/required interceptor enforcement. |
| `requirements.managed_cache_models` | Exact model IDs requiring admitted cache policy. |
| `requirements.managed_kinds` | Currently `primary` and `warming`; not auxiliary summaries. |

Keepalive and native compaction remain explicitly unavailable in this API.
Ordinary inference and its stream stay with the harness. The library does not
create pings, spend inference quota, enroll a session or retry an inference request
on its own. GPT retention estimates are UI observations, not eviction evidence.

Quota, prices, watches and per-chat previews retain the existing
[client protocol](client-protocol.md). Cache commands retain the durable
[revision-2 contract](cache-policy-contract.md). They use private, random
per-session capabilities, operation IDs, revisions and transition-bound receipts.
Discovery never grants authority to create those receipts.

## Account grants and the host boundary

Omitting `client_access` retains the shared pool. An explicit empty list grants
nobody access. To distinguish three people, give them separate ordinary CPA keys;
sharing one key also shares its authenticated principal and grants.

```yaml
# CPA host configuration, with the patched required-interceptor support:
plugins:
  required-interceptors: [pacer]

# Pacer plugin configuration (separate file):
enabled: true
client_access:
  grants:
    - caller_scope: <64-character lowercase SHA-256 digest>
      auth_ids: [claude-a.json, claude-b.json]
```

For CPA's built-in API-key provider the caller scope is
`SHA256("cli-proxy-api:caller-scope:v1\0" + trim(apiKey))`. Derive it locally,
without putting keys in shell arguments or logs. Other authentication providers
need an equivalent trusted-principal mapping; this API does not assume their
principal equals the raw key.

Grants filter credential rows, shared aliases, current/active identities and
next-use previews, and gate prices, policy controls and actual selected accounts.
The server reauthenticates ordinary-key watches after waiting. Capability reads
recheck their persisted owner; old ownerless bindings are unavailable in granted
mode until a new primary request establishes ownership. Changing grants requires
an idle plugin reload. Grants are admission constraints, not a replacement account
scheduler: if CPA selects an ungranted credential, the request is rejected before
dispatch. Configure the CPA pool accordingly.

Explicit grants require a host-generated
`host_capabilities.required_interceptors` registration guarantee. The local CPA
patch rejects missing, disabled, fused, failed or skipped required interceptors,
including nested execution and retries. A plugin cannot provide this guarantee
by itself. Older hosts continue to work in shared mode but cannot enable grants.
An operator or another trusted native plugin remains inside the server trust
boundary; this is not a sandbox against server administrators.

## Optional mandatory cache policy

With an initialized schema-7 cache store and required host enforcement:

```yaml
cache_policy:
  enabled: true
  require_managed: true
  routes:
    - id: claude-main
      models: [claude-opus-5-5, claude-sonnet-5-5]
```

For those models, both request admission and each actual selected dispatch require
a real policy selection previously acknowledged by Pacer. Merely sending headers
is insufficient. Model thinking suffixes cannot bypass the check; selected upstream
names are checked after alias resolution as well. Managed enrollment still requires
the configured exact model IDs: aliases and unsupported transports may be refused.

Missing admission returns `409 pacer_cache_policy_required`, with
`action: enroll_cache_policy` and `retryable: false`. The client must fix the
prerequisite before issuing a new request; replaying the same request cannot help.
New capability/access errors also include stable `code`, `action` and `retryable`
fields. Existing quota and cache errors retain their established wire format.

This option defaults to false. Stock OpenCode and the passive Pi example do not
automatically enroll managed cache sessions. Enable it only for a bridge that
implements the durable revision-2 contract. Keep a separate helper model outside
the required list: managed summaries/title requests are not supported yet, and
there is deliberately no header-only auxiliary exemption. Same-model compaction
will be refused. A false declaration must not bypass admission.

The host explicit-cache fix honors `prompt_cache_options.mode: explicit` for
auxiliary requests: no automatic breakpoints are added. The caller must remove
its own cache markers. This constrains outgoing cache instructions; it does not
claim to control provider-side billing or guarantee a cache hit. Warmth estimates
and guesses about the cheapest compaction moment are not hard refusal rules.

## A harness bridge

1. Resolve the exact CPA origin and key. Scope state by connection, installation,
   session and route. Do not send capabilities or keys to redirects or other APIs.
2. Read discovery. Declare only implemented features on each request. Bind serving
   account headers to that exact response, not to the next-use preview.
3. Use one cancellable quota watch. Replace authoritative account snapshots and
   render unknown/stale quota as unknown/stale; do not invent a reset or full balance.
4. If supporting switch recovery, compact only at a safe host step boundary.
   Report a checkpoint only after durable commit. An opaque provider-specific GPT
   summary is not automatically portable between accounts.
5. Keep one logical step identity across retries. Stop, connection changes and
   unload invalidate recovery. Check cancellation again before dispatch.

The optional `RecoveryController` provides one recovery budget per logical step
and a session lock that survives cancellation until the host callback settles.
It does not send inference. A returned checkpoint is a host attestation, not proof
that the server inspected local history. Uncertain upstream failures must never
be interpreted as permission to replay paid work.

The Pi example is intentionally passive: current Pi hooks lack the response body
and request identity needed for safe automatic handoff recovery. Richer integration
requires a tested provider bridge. Do not advertise a capability to hide that gap.

## Build evidence

`scripts/build.sh` embeds the plugin version/revision/dirty status and writes an
artifact SHA-256 receipt to `dist/linux_<arch>/build.json`. A source revision and
binary hash identify artifacts; they do not retroactively prove how an existing
production binary was built. See [the recovered-source receipt](live-build-2026-10-12.md).
