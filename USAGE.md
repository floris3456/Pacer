# Credential usage resource (plugin 0.1.2)

OpenCodePlus 1.1.2 adds `/usage` and `/usage --all`. This extension reuses the
existing `GET /v0/resource/plugins/quota-handoff/events` route:

```text
?view=usage&model=<OpenCode-provider>/<model>&all=false
Authorization: Bearer <existing-session-route-capability>
```

`all=false` returns every configured credential for the binding's resolved CPA
provider, with general, matching model-specific, and OAuth-app quota windows.
`all=true` returns all configured credentials and recorded windows across
providers. Only the plugin's configured Claude/Codex credential pool is covered.

The JSON response has `protocol: 1`, `view: "usage"`, server `now`,
`max_age_seconds`, scope `all`, resolved `provider`/`model`, `current`, `active`,
`last_used`, and `credentials`. Each credential has its exact `id`, display
`alias`, `provider`, `shared_with` aliases, and normalized quota `windows`.

`active` lists actual host-confirmed credentials for running primary requests on
this chat/model. `current` is the most recently confirmed primary credential;
when idle the UI labels it LAST USED. Scheduler candidates alone, other chats,
title generation and compaction cannot overwrite this identity. Last-used state
survives plugin restarts; running state does not survive a process restart.

The raw capability must match a registered binding and the exact `model` route.
Anonymous reads, unknown capabilities and mismatched routes receive 401. Invalid
`all` values receive 400. Responses are `Cache-Control: no-store`. Do not copy
capabilities or keys from runtime state into documentation or evidence.

Before the selected chat/model has made a request, OpenCodePlus defaults `/usage`
to all credentials. It sends the configured CPA API key with explicit
`auth=api-key&all=true` on the same route. CPA validates the key through its own
loopback `/v1/models` endpoint without executing a model. The endpoint must reject
anonymous requests and accept the supplied key; redirects are refused and keys
are never cached. The result has empty `current`/`active` and creates no binding.
API-key reads cannot request model-scoped usage. Management keys are not used.

`usage_auth_url` defaults to `http://127.0.0.1:8317/v1/models`. Set it to the CPA
listener's port if that differs. Only an HTTP loopback literal, explicit port and
exact `/v1/models` path are accepted. No Caddy change is required. OpenCodePlus
keeps the API key inside the host and requires the model base URL and quota origin
to match before sending it.

A read does not renew activity, enroll a chat, acknowledge a notice, reserve
capacity, compact or switch credentials. After authorization, it may request the
existing quota sampler to refresh asynchronously under its rate limits/backoff.
Missing or stale evidence is preserved explicitly. Once the selected chat/model
makes an ordinary authenticated CPA request, its existing capability gives the
model-scoped view and actual credential identity. Missing server bindings fall
back to the API-key all-credentials view without enrollment.

No CPA core, CPAMP or Caddy source/configuration changes are required. Deployment
requires replacing the plugin with 0.1.2 through the existing controlled plugin
load procedure, then using the corresponding OpenCodePlus build. These source
changes do not deploy or enable the candidate themselves.

## Verification

- `internal/handoff/usage_test.go`: fake Claude/Codex credentials, shared quota,
  applicable windows, exact host identity, another chat, auxiliary requests,
  restart, invalid authorization, and byte-identical state around reads.
- `internal/bridge/usage_test.go`: actual plugin request interception and resource
  callbacks with dummy credentials, scoped/all responses, authentication refusal,
  invalid arguments, and preserved anonymous protocol discovery.
- OpenCodePlus `packages/plus/test/quota/usage*`: HTTP capability reads, old-plugin
  handling, redirect refusal, slash command dispatch, rendered bars and scrolling.

- `internal/bridge/usage_auth_test.go`: fresh-chat all view, ordinary key
  validation, invalid/revoked key refusal, open/redirected endpoint refusal,
  loopback configuration and unchanged durable state.
