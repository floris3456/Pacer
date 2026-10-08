# Opt-in assigned response-tier fixture

New fixture processes start with `response_service_tier = None`. Requests never
choose their own assigned response tier. Existing request bodies, attempts, response
text, quota/exhaustion state and stream-hold controls keep their existing behavior.

## Control contract

`GET /_test/service-tier` returns exactly `{"service_tier": null}`, or the current
configured `standard` / `priority` string.

`POST /_test/service-tier` accepts only:

- `{"service_tier":"standard"}`
- `{"service_tier":"priority"}`
- `{"service_tier":null}` or `{}` to clear

Extra keys, other strings/case/whitespace and non-string non-null values return
400 `invalid_fixture_service_tier`; active inference returns 409 `fixture_busy`.
A successful update returns the same current-mode object as GET. The control does
not reset attempts, analytics counters, reply text, exhaustion or hold state.

When configured, JSON replies and SSE `message_start.message.usage` include the
exact `service_tier`. When cleared, that key is omitted. The final SSE usage-only
delta remains `{"output_tokens":4}`: CPA must preserve the prior optional tier while
merging counts. CPA source tests additionally cover tagged final updates and conflicts.

Observed counts stay 10 input / 4 output / 500 read / 25 write-5m / 75 write-1h.
The explicit synthetic forward book (input 4, output 20, read 0.2 USD/million)
values a separately observed `standard` event at 0.000945 USD. This fixture does not
assert a real provider's model/tier availability or invoice pricing.

## Runtime handoff is intentionally pending

Editing this script does not reload the currently running fixture. **Do not restart
the original fixture to load this code:** its 44-attempt memory would be lost. No
snapshot/restore endpoint, reload mechanism or injecting proxy was added.

Parent must choose and authorize the runtime preservation plan first. A separately
owned new fake listener could retain the original process untouched, but its fresh
counter domain must be recorded independently; its attempts cannot be presented as
continuation of the original 44. Any dummy credential endpoint change is parent-owned.
This source handoff does not select or execute that runtime operation.

Once the parent has an authorized listener with this version, read and retain the
current mode, set `standard` only for the selected positive test, and restore the
original mode (normally null) in `finally`, after all active messages have closed.
Preserve every old unvalued row and attempt. A newly assigned tier belongs to a new
physical event; replay cannot silently revise the old captured identity.

Offline verification: `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest -v test_upstream`
from this directory uses only fresh ephemeral loopback test servers. The separate
`internal/measurement/response_tier_test.go` exercises actual SDK wire → `FromNative`
→ immutable feed → forward library, including unvalued old record preservation.
