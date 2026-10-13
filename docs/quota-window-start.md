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
  model: claude-haiku-5-5
```

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
- The host builds a fixed request with one output token, thinking disabled,
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
- Ambiguous errors and interrupted dispatches remain unconfirmed. Another
  request requires observing an active window, its expiration, then fresh
  dormant evidence. Only a proven non-dispatch may retry after 15 minutes.
- Ordinary quota polling retains its shared pacing and provider backoff. This
  feature does not create a separate aggressive quota-polling loop. Detection
  therefore follows the existing poll schedule, not an exact reset-time alarm.

The authenticated usage API adds `quota_window_start` to each already-authorized
account row, including state and the last attempt. Management quota reads expose
the same status. Clients cannot trigger a start. Feature discovery advertises
`claude_quota_window_start` independently of cache keepalive.

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
