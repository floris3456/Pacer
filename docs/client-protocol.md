# Pacer and its clients

Pacer works with any client. A client that knows Pacer (the opencode plugin, on stock opencode, opencode V2 or
OpenCodePlus) declares on each request what it handles, and Pacer only asks a client for what it declared. A client
without the plugin gets CPA's ordinary behaviour: when its chat moves to another account it simply continues there
and the new account builds its cache from scratch.

## Discovery

`GET /v0/resource/plugins/pacer/events` without `Authorization` answers, among the older fields:

```json
{"protocol":1,"pacer":true,
 "client":{"version":1,"request_header":"X-Pacer-Client","caps":["quota"],
           "quota_header":"X-Pacer-Quota","views":["usage","prices"]}}
```

Behind an SSO proxy, this exact GET path has to reach CPA without SSO, like the older quota-handoff path
([caddy.md](caddy.md)).

## Declaring a client: `X-Pacer-Client`

Sent on each model request, never per API key (one key is often shared by clients with and without the plugin):

```text
X-Pacer-Client: v=1; caps=quota
```

- `caps=quota`: the reply carries `X-Pacer-Quota`.
- Unknown capabilities are ignored; an unknown version or a malformed value counts as no declaration. A request is
  never refused because of this header.
- CPA removes the header before the request goes upstream.

Compacting before an account switch is separate: only a client that can compact at a step boundary sends the
`X-Quota-Handoff` claim with `compact_on_switch` (OpenCodePlus). Pacer answers `409 quota_compact_required` only to
such requests.

## Quota on replies: `X-Pacer-Quota`

For a declared request, the reply (or the first chunk of a stream) carries the serving account's quota windows. Pacer
builds it after reading the reply's own quota headers, so it already includes them. The value is base64url (no
padding) of:

```json
{"v":1,"provider":"claude","auth":"claude-x.json","alias":"X","at_ms":1791587233865,
 "windows":[{"scope":"all","seconds":18000,"left":60,"reset_ms":1791594433000,
             "reset_quality":"absolute","state":"current","read_ms":1791587233865,
             "source":"header","applies":true}]}
```

- `left` is the % left. Times are Unix milliseconds. `reset_quality` is `absolute` (the provider's timestamp) or
  `estimated` (from a countdown).
- `state`: `current`, `estimated`, `stale` (balance older than an hour), `dormant`, `partial`, `unknown`,
  `awaiting-confirmation` (the reset time passed and no reading confirmed the new window yet), `ambiguous`,
  `conflicting`. Show a countdown to `reset_ms`; when it reaches zero, show "reset due" until a reading confirms it.
  Never invent a full window.
- `applies` says whether the window limits the request's model.
- Windows the provider reports as not applying to the account are left out. Antigravity and OpenCode Go accounts are
  included; their windows come from Pacer's meter reads (`source: api`).
- Upstreams cannot set this header: CPA removes any upstream `X-Pacer-Quota` before Pacer adds its own.

## Views (`Authorization: Bearer <CPA API key>`, `auth=api-key`)

Pacer checks the key against CPA's own loopback `/v1/models`; reads never call a provider.

- `?view=usage&auth=api-key&all=true&v=1`: every account with a meter (Claude, Codex, Antigravity, OpenCode Go), with
  IDs, aliases and windows in the `X-Pacer-Quota` format (all states, including stale and not-yet-reset windows).
  Without `v=1` the windows use the older format, which leaves out windows without a fresh balance and reset. This is
  a shared-pool view, not per-tenant.
- `?view=prices&auth=api-key`: Pacer's price for every model the key can use, as on Model Settings (manual, else the
  imported book, else models.dev), so a client's cost display matches Pacer's usage page:

  ```json
  {"protocol":1,"view":"prices","unit":"usd_per_million_tokens","revision":3,"synced_at_ms":1791500000000,
   "models":{"claude-opus-5-5":{"input":5,"output":25,"cache_read":0.5,"cache_write":6.25,"source":"imported"}}}
  ```

  `cache_write` is the 5-minute write price; `context_tiers` and `fast` appear when the model has them. Models without
  a price are left out.

## How often Pacer reads meters

Reads of a provider's quota endpoint are Pacer's alone; clients and pages only read Pacer's copy
([observation-contract.md](observation-contract.md)). Claude and Codex report quota with their responses, so an
account in use needs no reads; an idle one is read every 30 minutes, and once just after a reset. Antigravity and
OpenCode Go report nothing with responses: they are read every 30 minutes and 5 minutes after a burst of requests.
No account is read more than once in 15 minutes.
