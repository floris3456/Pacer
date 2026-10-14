# Dormant five-hour quota windows

This optional **server-side** feature sends one minimal Claude Haiku request
when provider evidence confirms that an account's five-hour window has not
started. It does not increase quota or extend an existing window. Its purpose
is to let unused windows expire sooner; it can also waste a small amount of
included usage if nobody subsequently uses the account.

It is separate from cache keepalive, uses no conversation, and never sends
GPT/subagent keepalive requests. A metadata ping is not inference and cannot
reliably start a usage window.

## Configuration

Under `plugins.configs.pacer`:

```yaml
quota_window_start:
  enabled: true  # Omitted/false is the default.
  model: claude-haiku-4-5-20251001
```

The default uses the minimal Haiku 4.5 helper profile verified against the
provider: one text message, one output token and no thinking or cache fields.
This internal model selection does not change chat or compaction models.
An explicit Haiku 5.5 override remains supported and disables adaptive thinking;
its availability must be verified for the intended accounts.

The paired CPA host must advertise the exact-account, single-attempt, no-cache
capability. Enabling on an older host fails closed. Changing this configuration
requires an idle reload. Never delete or roll back its private
`state_path.window-start.sqlite` ledger to retry a request.

## Dispatch conditions and guarantees

- Only enabled, verified first-party Claude OAuth accounts which belong to at
  least one configured caller grant qualify. Aliases share the billing-account
  ledger. An account serving an observed user request is skipped.
- A single fresh API snapshot (under two minutes old) must explicitly report
  five-hour utilization `0` and `resets_at: null`. Missing, stale, conflicting,
  expired or merely zero-used active windows are not dormant.
- The weekly allowance must be known, and all applicable active limits must
  retain more than one percentage point. The same API snapshot must explicitly
  say `extra_usage.is_enabled: false`. Unknown or enabled paid overage blocks
  automatic starts. An external change to that setting between observation and
  dispatch cannot be atomically prevented by the provider API.
- The host builds a fixed request with one output token and no reasoning,
  no tools, no history and no cache-write instructions. It pins the verified
  account and does not retry, refresh-and-resend or fall back to another account.
- The durable claim precedes dispatch; three users, aliases and restarts cannot
  multiply it. Globally, at most one start is attempted every 30 seconds.
- Each operation has an immutable account capture and its own usage correlation
  across host restarts. It records the small usage without joining a user's
  session or changing that session's cache statistics.
- A successful HTTP response is **not** proof of a started timer. Only a newer,
  absolute provider reset confirms it. The existing quota UI displays that real
  reset; no five-hour deadline is fabricated.
- A received HTTP429 is a refusal, including when its error body is truncated.
  It may retry after 15 minutes, then 30 minutes; a longer `Retry-After` always
  wins. An explicit provider spend-cap error blocks retries. Proven non-dispatch
  follows the same schedule. Each verified billing account has a durable budget
  of three attempts per five hours; the global 30-second spacing still applies.
  Every retry requires eligible dormant evidence newer than the previous finish.
- Ambiguous network errors, interrupted dispatches without a response, HTTP5xx
  and successful responses without timer confirmation remain fenced. Time alone
  never authorizes their replay. A new start requires observing an active window,
  its expiration, then fresh dormant evidence.
- On upgrade, completed legacy HTTP429 attempts become retryable without
  deleting their original timestamps or budget. Already confirmed attempts are
  unchanged. The server persists bounded diagnostic codes, never response bodies.
- Ordinary quota polling retains its shared pacing and provider backoff. This
  feature does not create a separate aggressive quota-polling loop. Detection
  therefore follows the existing poll schedule, not an exact reset-time alarm.

The authenticated usage API adds `quota_window_start` to each already-authorized
account row, including state and the last attempt. Management quota reads expose
the same status. Clients cannot trigger a start. Feature discovery advertises
`claude_quota_window_start` independently of cache keepalive.

Clients can display the retry deadline and attempt budget separately from the
provider reset. For example, `Start: Retry in 15m · 429` describes a refused start,
not a running five-hour window. A retry deadline is a minimum: fresh quota and
the other dispatch conditions are still required when it expires.

## Provider scope

Claude's API has an explicit dormant representation. ChatGPT/Codex's current
quota adapter does not provide equivalent reliable evidence, and some plans
have no five-hour limit. Codex, Google, API-key and compatibility-provider
accounts are therefore not automatically pinged by this feature.

Provider references (reviewed 2026-10-13):

- [Claude usage and length limits](https://support.claude.com/en/articles/11647753-how-do-usage-and-length-limits-work)
- [Claude usage best practices](https://support.claude.com/en/articles/9797557-usage-limit-best-practices)
- [ChatGPT/Codex pricing and limits](https://learn.chatgpt.com/docs/pricing)

## Acceptance

Validate explicit dormant/unknown/active states, weekly exhaustion, paid-usage
settings, duplicate credentials and concurrent callers, restart/crash recovery,
ambiguous responses, confirmation delay, cancellation and the real final wire
through a nonforwarding fake provider. Keep changes staged while the user's
Opus task runs; no real inference or live service restart is part of preparation.
