# Codex opaque-state origin guard

The generic host and Pacer use the private `codex_origin.attempt` contract to
keep encrypted Responses state on its observed account. This does not depend on
a session ID, a client-declared account, managed-cache enrollment, or a model
alias. Ordinary plaintext requests retain their existing account pool.

Before scheduling, Pacer finds the common owner of retained compaction,
encrypted reasoning, item-reference, previous-response, and conversation state.
It intersects that one credential with the authenticated caller's existing
grant. An unknown, expired, mixed, or unauthorized origin is rejected. An
unavailable owner cannot fall back to another credential.

The physical Codex HTTP executor checks both the original body and the final
wire body. Its binding includes the actual runtime credential ID, provider
account identity, effective account header, and executor endpoint. It verifies
the final Authorization header against the selected credential. The shared
runtime credential guard prevents replacement or refresh between validation
and dispatch; it releases after `WroteRequest` or transport return.

Only a successful native `/responses/compact` response or completed Responses
event enrolls output. Missing streamed output is reconstructed from completed
items before enrollment. Failed, incomplete, or truncated attempts enroll
nothing. Streamed items received before completion do not authorize replay.
Binding uses the successful attempt, including when an earlier account failed.
The marker transaction commits before the final successful event is returned.

The host callback pins authenticated caller scope, logical request ID, and the
plugin snapshot. A fused, replaced, or unavailable plugin fails closed.
Guarded WebSocket executions use the existing HTTP implementation; a request
requiring upstream WebSocket replay is rejected instead of bypassing the guard.
Native compact support is advertised only with the host's version 1 physical
origin contract and the corresponding model evidence.

## Storage

The owner-only `STATE_PATH.codex-origin/origins.sqlite` sidecar holds a random
HMAC key, keyed marker/account digests, credential IDs, and expiry times. It
contains no ciphertext, prompt text, raw account ID, caller key, or auth token.
The directory requires mode 0700 and files mode 0600. Public directories and
symlink database paths fail closed. Retention is 30 days and the store is bounded
to 100,000 markers. No schema changes run against Pacer's main state database.

Deleting or losing this sidecar intentionally makes previously issued state
unknown. A portable plaintext checkpoint can resume on a different account.

## Validation handoff

No live provider calls, deployment, or commits were performed. Tests use a
local HTTP fake and temporary SQLite files.

- Pacer: `go test -tags sqlite_fts5 ./internal/origin ./internal/pacer -run 'Test.*Origin' -count=1`.
- Host: `go test ./internal/runtime/executor ./internal/pluginhost ./sdk/api/handlers -run 'TestCodexOrigin' -count=1`.
- The tests cover successful-account attribution after failure, native compact
  model/path preservation, same-origin reuse, rejection before network dispatch,
  plaintext routing, streamed reasoning reconstruction, incomplete/truncated
  responses, credential replacement at dispatch, plugin replacement/fuse/panic,
  immutable authenticated caller, grants, restart, expiry, atomic conflict,
  strict ambiguous-payload parsing, sidecar permissions/privacy, and main SQLite
  schema cookie 70.
- Focused logs: `/tmp/pacer-codex-origin-tests.log` and
  `/tmp/cpa-codex-origin-focused.log`.

Integration still requires the final combined host build and the surrounding
discovery/replay checks after concurrent work is complete. Guarded HTTP is the
supported execution path; upstream-only WebSocket replay is deliberately not
advertised by this contract.
