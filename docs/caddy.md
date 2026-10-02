# Caddy and OpenCodePlus connection

```text
OpenCodePlus host -- HTTPS + CPA API key --> Caddy --> CPA inference
OpenCodePlus host -- HTTPS + capability --> Caddy --> plugin events/usage
CPA plugin       -- private backend token --> compatible CPAMP (optional)
```

Every user connects to the same CPA HTTPS origin. OpenCodePlus initiates polling;
CPA does not dial the user's machine and no client-side port needs exposure.

## Caddy

[examples/Caddyfile](../examples/Caddyfile) is a complete minimal configuration
with normal CPA API-key authentication for `/v1/*` and no public administration.
Replace the hostname and upstream with yours. Caddy handles TLS normally.

For an existing PocketID/SSO deployment, keep your current inference and admin
routes. Insert the following handler **inside the same ordered `route` block,
before its SSO/catch-all handler**:

```caddyfile
@quota_events {
    method GET
    path /v0/resource/plugins/quota-handoff/events
}
handle @quota_events {
    reverse_proxy 127.0.0.1:8317 {
        header_up -Remote-User
        header_up -Remote-Email
        header_up -Remote-Groups
        header_up -Remote-Name
        header_up -X-Auth-Request-User
        header_up -X-Auth-Request-Email
        header_up -X-Auth-Request-Groups
        header_up -X-Authenticated-User
        header_up -X-Forwarded-User
    }
}
# Your existing inference and SSO-protected catch-all handlers follow here.
```

This is ordinary Caddy syntax and needs no custom imports. Strip any additional
identity headers your existing SSO trusts. Preserve the client's `Authorization`
header: Caddy forwards it by default. **Never inject a management key into this
route.** Do not exempt `/v0/*`, `/v0/management/*` or `/internal/*` from SSO.
The exact GET path serves discovery, authenticated events and authenticated usage;
query parameters do not need separate routes. Other methods keep their existing
protection. Validate and reload with your normal Caddy service procedure.

## OpenCodePlus

Use OpenCodePlus `v2.0.18-plus-1.1.2` or newer for `/usage`. Configure your usual
CPA model provider with its normal API key, base URL such as
`https://proxy.example.com/v1`, and HTTP transport (`settings.transport: "http"`
on that provider). Then create:

`~/.config/opencodeplus/quota-handoff.json`

```json
{
  "routes": {
    "YOUR_PROVIDER_ID": "https://proxy.example.com"
  }
}
```

`YOUR_PROVIDER_ID` is the provider key in your OpenCodePlus configuration, such
as `cliproxyapi`; it is not the model ID or the underlying Claude/Codex name.
Use the public **origin**, without `/v1` or the event path. The model base URL
and quota origin must match. If `XDG_CONFIG_HOME` is customized, use its
`opencodeplus` directory. Reload the OpenCodePlus host through its normal service
procedure to load the configuration; reconnect its TUI.

## Authentication and compaction

1. A normal inference request is authenticated by CPA with the user's API key.
   OpenCodePlus adds coordination metadata describing installation, chat, model
   route and a SHA-256 digest of a private capability. The raw capability is not
   included in inference headers. CPA strips coordination headers before upstream.
2. OpenCodePlus polls the event URL using the raw capability. It must match the
   registered chat/model route. The plugin returns reminders or compaction intent.
3. In enforcement mode, with automatic quota compaction enabled on the client,
   the client compacts as soon as its session can process the control action.
   Subsequent authenticated requests carry the completed checkpoint and intent.
   CPA switches that chat only after the acknowledgement. Other chats retain
   their own bindings while completing their compaction.
4. In shadow mode, the message tells the user to run `/compact` or start a fresh
   chat. Quota compaction and handoff are disabled. Compaction never resets quota.

Before a chat/model has sent a request, no capability binding exists. `/usage`
defaults to all credentials and uses the configured **CPA frontend API key**:

```text
GET /v0/resource/plugins/quota-handoff/events?view=usage&auth=api-key&all=true&model=PROVIDER%2FMODEL
Authorization: Bearer <CPA frontend API key>
```

CPA validates it against its own loopback `/v1/models` endpoint without inference.
Any valid frontend key can see IDs, aliases and quota bars for every configured
plugin credential. This is a shared-pool view, not tenant-isolated reporting.
Reads do not enroll a chat, compact, reserve quota or change routing. Once a model
request establishes a binding, `/usage` scopes to that provider and shows IN USE
or LAST USED based on actual host execution.

## Verify

```sh
curl -fsS https://proxy.example.com/v0/resource/plugins/quota-handoff/events
```

Expected in shadow mode: `{"mode":"shadow","protocol":1}`. Anonymous discovery
reveals only protocol/mode; anonymous usage and invalid capabilities must return
401. An HTML login page means the handler is still behind SSO. A 404 usually means
the plugin is not loaded or the exact route/upstream is wrong. A fresh `/usage`
authentication failure can mean a bad CPA key, a base URL/origin mismatch, or an
incorrect/anonymous `usage_auth_url`. Check protected management URLs still
require your existing authentication after changing Caddy.
