# Pacer and its clients

Pacer works with any client. A client that knows Pacer (the opencode plugin, on stock opencode, opencode V2 or
OpenCodePlus) declares on each request what it handles, and Pacer only asks a client for what it declared. A client
without the plugin gets CPA's ordinary behaviour: when its chat moves to another account it simply continues there
and the new account builds its cache from scratch.

This describes the default shared mode. Optional account grants and mandatory
cache admission can restrict requests. See [Independent clients](independent-clients.md)
for the generic API, host guarantees and bridge responsibilities.

## Discovery

`GET /v0/resource/plugins/pacer/events` without `Authorization` answers, among the older fields:

```json
{"protocol":1,"pacer":true,
 "client":{"version":1,"request_header":"X-Pacer-Client","caps":["quota"],
            "quota_header":"X-Pacer-Quota","views":["usage","prices","usage-watch","capabilities"]}}
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

Automatic primary claims stay within their previously bound provider, even when
CPA's alias can route to several providers and the caller is granted accounts on
all of them. A cross-provider selection returns
`409 quota_provider_switch_forbidden`, with `action: select_same_provider` and
`retryable: false`. The client must surface that refusal; compaction or automatic
replay cannot resolve it. Same-provider model and account switches keep the
existing checkpoint rules. An intentional provider change needs an explicit
manual choice or a new route; a claim with historical bindings on both providers
cannot resume automatic routing. Caller, installation, session and route define
this boundary independently of the private capability.
Claimed bindings currently support Claude and Codex credentials. Other provider
inventory is for usage display only; a claimed fallback to one of those
credentials is refused before dispatch with `quota_state_unavailable`.

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

### Codex credits

Codex replies and usage credentials also carry an optional, separate `credits`:

```json
{"state":"available","read_ms":1791806400000,"expires_ms":1791810000000,"source":"api"}
```

States are `available`, `empty`, `unlimited`, `unknown`, and `stale`. These are
provider credit flags, not a dollar amount, quota percentage, reset countdown,
or a guarantee that a particular inference request will succeed. Subscription
windows remain unchanged: **0% weekly quota can coexist with available credits**.
`unlimited` refers only to credits, never to every account or model limit.

The existing scheduled Codex usage read supplies this field; UI reads, quota
headers and watch requests make no additional provider calls. Missing/malformed
credit flags in a successful reading become `unknown`. Before any reading,
`unknown` has no timestamps. Otherwise `read_ms` is the original observation and
`expires_ms` is its one-hour freshness boundary. Clients must age it locally,
including across persistence/reconnects, and must not substitute the envelope's
`at_ms`/`now_ms`. Passive quota headers can postpone a direct usage read; credits
then become visibly stale until the next scheduled read, rather than causing
additional polling. Source freshness does not reserve credits against other users.

Credit observations use an owner-only `<state_path>.credits.sqlite` file. They
never change the main measurement ledger's guarded schema generation. Missing
or unavailable credit storage degrades to unknown credits while ordinary quota
windows and policy enforcement remain available.

Merge credit evidence by `read_ms` independently from quota-window envelopes;
an absent field on an older server/header must not refresh or erase evidence.
Retain account-grant filtering for aliases and persisted data. Display credits
as their own status line, without changing the subscription bars' percentages.

Quota-based routing retains an already-bound, currently offered Codex account
when a subscription floor is reached unless credits are explicitly fresh-empty.
The advisory reason is `credit_status_unscored`: Pacer cannot convert credits
into comparable placement capacity. Removed, cooling, disabled, or ungranted
accounts remain ineligible. New placements receive no invented credit score.
This does not enable active routing or change provider error handling.

## Views (`Authorization: Bearer <CPA API key>`, `auth=api-key`)

Pacer checks the key against CPA's own loopback `/v1/models`; reads never call a provider.

- `?view=usage&auth=api-key&all=true&v=1`: every account with a meter (Claude, Codex, Antigravity, OpenCode Go), with
  IDs, aliases and windows in the `X-Pacer-Quota` format (all states, including stale and not-yet-reset windows).
  Without `v=1` the windows use the older format, which leaves out windows without a fresh balance and reset. This is
  a shared-pool view by default. With `client_access`, only granted accounts and
  their aliases are included.
- `?view=prices&auth=api-key`: Pacer's price for every model the key can use, as on Model Settings (manual, else the
  imported book, else models.dev), so a client's cost display matches Pacer's usage page:

  ```json
  {"protocol":1,"view":"prices","unit":"usd_per_million_tokens","revision":3,"synced_at_ms":1791500000000,
   "models":{"claude-opus-5-5":{"input":5,"output":25,"cache_read":0.5,"cache_write":6.25,"source":"imported"}}}
  ```

  `cache_write` is the 5-minute write price; `context_tiers` and `fast` appear when the model has them. Models without
  a price are left out.

### Shared credential change subscription

The usage view includes `now_ms` (millisecond snapshot time) and an opaque
`cursor`. To subscribe, repeat the authenticated usage GET with
`&cursor=<last cursor>&wait_ms=20000`. Pacer returns immediately when any caller's
response headers or a scheduled provider read updates its stored observations.
No model request from the viewer is needed, and the caller contributing headers
does not need to declare `X-Pacer-Client`. Primary/last-used identity stays local
to the viewer's chat; shared quota changes do not change it.

Native resource responses are finite JSON, so this is a bounded long poll, not
SSE. An initial/mismatched cursor returns a full snapshot immediately. An
unchanged cursor returns a heartbeat snapshot after at most 20 seconds. Cursors
are instance-specific, so restart/reconnect cannot silently lose updates.
Capture the returned cursor and rearm the request; coalesced changes are covered
by the next full snapshot. Do not run overlapping waits. Retry failures with
backoff; older servers without a cursor require ordinary cached-view polling.

Notifications occur after storing observations, before the rest of measurement
processing. They never block on viewers, perform provider requests, or run on
payload chunks. Authentication still uses CPA's local model-list boundary.
The native ABI cannot cancel a held call on HTTP disconnect; held calls therefore
have the 20-second lifetime and a global limit of 256 concurrent waiters.

## How often Pacer reads meters

### Per-chat next-use preview

`GET /v0/resource/plugins/pacer/events?view=usage&v=1&model=<client-provider/model>`
with `Authorization: Bearer <private chat capability>` adds `next_use` to the usage
view. It is scoped by the existing binding capability and route; the shared API-key
view does not expose other chats' routing preferences. The raw capability is never
returned. Older clients can ignore this additive field.

```json
{"v":1,"at_ms":1791587233865,"expires_ms":1791587263865,
 "candidates_at_ms":1791587200000,"mode":"dry-run","provider":"claude",
 "model":"claude-opus-5-5","reason":"cold_move","auth":"claude-y.json",
 "alias":"Y","forced":false}
```

This is a **proposal, not a reservation or a promise of CPA's next selection**.
Each read reruns the same routing scorer with current observed quotas, measured
capacity/runway, and the chat's real cache protection and in-flight state. Economic
changes (`cold_move`) are hidden while protection is warm or unknown. Exhaustion
at the scorer's hard floors can yield `forced_move` even while warm. It does not
invent an exhaustion forecast from missing measurements.

Candidate eligibility comes from that chat's last primary scheduler invocation
(`candidates_at_ms`), intersected with the current discovered credential inventory.
CPA revalidates cooldowns, model eligibility and availability on actual dispatch.
No candidate snapshot after restart, one older than 24 hours, missing quota
evidence, or routing off means no target. The in-memory snapshot cache is bounded.
The `auth` and `alias` fields are absent whenever no replacement is proposed.

Render **NEXT USE** separately from **LAST USED**, and explicitly label `dry-run`
proposals: routing in this mode remains CPA's choice. Hide a preview after
`expires_ms`, on a failed read, or when switching chat/model. Visible sidebars may
refresh every 15 seconds. Reads only consult local state: they do not enroll,
admit, move, compact, reserve capacity or fetch provider quotas.

### Live request percentages

The response interceptor observes the upstream percentage headers **before**
building `X-Pacer-Quota`. For streaming requests this happens at header init,
before the first content chunk. Payload chunks do no quota work. Clients should
apply the header immediately and push their local UI update rather than waiting
for the usage poll; auxiliary replies update meters but not the chat's LAST USED
identity. This adds no network request per inference response.

Reads of a provider's quota endpoint are Pacer's alone; clients and pages only read Pacer's copy
([observation-contract.md](observation-contract.md)). Claude and Codex report quota with their responses, so an
account in use needs no reads; an idle one is read every 30 minutes, and once just after a reset. Antigravity and
OpenCode Go report nothing with responses: they are read every 30 minutes and 5 minutes after a burst of requests.
No account is read more than once in 15 minutes.
