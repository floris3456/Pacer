# Cache policy protocol 1 — internal integration contract

Implemented by `internal/cachepolicy`, independently of routing/account-switch generation.
No HTTP handler, executor rewrite, client controller, quota reservation or adaptive policy lives here.

## Host API (all structs use only standard-library types)

```go
Create(context.Context, *sql.DB, Options) (*Store, error)
Open(context.Context, *sql.DB, Options) (*Store, error)
Options{Clock func() time.Time, Routes []Route}
Route{ProviderRoute string, TTLs []TTL}
Credentials{CallerDigest string, Capability string}
Evidence{NewEmpty bool}

(*Store).Get(context.Context, Credentials, Scope) (State, error)
(*Store).Apply(context.Context, Credentials, Command, Evidence) (State, error)
(*Store).BeginRequest(context.Context, Credentials, Request) (Snapshot, error)
(*Store).BeginAttempt(context.Context, Credentials, Attempt) (Snapshot, error)
(*Store).ValidateAttempt(context.Context, Credentials, Snapshot) error
(*Store).GetAttempt(context.Context, Credentials, Attempt) (Snapshot, error)
(*Store).FinishAttempt(context.Context, Credentials, Attempt, Outcome) error
(*Store).FinishRequest(context.Context, Credentials, Scope, string /* requestID */, Outcome) error
(*Store).Outstanding(context.Context, Credentials, Scope) ([]Snapshot, error)
DecodeCommand(io.Reader) (Command, error)
```

`Scope{InstallationID, SessionID, ProviderRoute string}`. The host supplies the lowercase
64-hex SHA-256 **authenticated inference-caller identity digest**, never a JSON field or an
unverified incoming caller-identity header. Do not use a display alias or raw inference key.
A shared inference key means a shared caller scope. Key rotation/ownership transfer is unsupported.

`Credentials.Capability` comes separately from the protected `X-Pacer-Cache-Capability` header.
It is an unpadded base64url encoding of 32 cryptographically random bytes, generated/stored privately
by the client **before initial ensure**, so lost responses can be retried. Only its SHA-256 hash is
persisted. It is never in JSON responses, URLs, prompts or logs. Missing/invalid capability fails.
Neither credentials nor `Evidence` may be decoded from client operation JSON.

`Evidence.NewEmpty` is a trusted enrollment attestation, not `state missing`, `generation == 0`,
or a client JSON `enrollment` flag. The host must independently establish a genuinely empty session
through its trusted client/session integration. Without that integration initial enrollment returns
422. This conservative initial implementation does **not** support unknown/pre-existing history,
copied-context forks, route migration, or ownership transfer. A new independently verified empty
child with a new session ID can enroll. Existing use never becomes empty again.

`Routes` explicitly declares the TTLs the final executor actually supports. No default routes or
TTL downgrades exist. The host must additionally reject unsupported model/transport combinations
before admission. This package only knows provider-route support, not executor internals.

## SQLite setup and recovery

The caller owns/closes `*sql.DB`, installs its SQLite driver, and configures durable SQLite
(`_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000` with go-sqlite3 is tested).
`Create` is an explicit first-installation operation on a never-enrolled store; it refuses an
existing schema. **Always use `Open` on normal startup. Never fall back from `Open` to `Create`.**
Missing/unsupported schema is 503. Existing tables are not migrated or modified by `Open`.
Tables are prefixed `cache_policy_`; unrelated legacy tables are untouched.

Writes acquire the SQLite writer lock before reading policy, giving cross-Store/process CAS
serialization without depending on a process mutex or deferred read-to-write lock upgrade.
SQLite I/O/lock/context failures are 503, not successful admissions. The injected clock is UTC
normalized; it is required. The store does not close the database or change its connection pool.

Every active request/physical-attempt guard survives restart. No timeout, restart or cancellation
claim automatically settles it. `Outstanding` inventories one authorized scope. Only after the
host confirms old dispatch stopped may it call `FinishAttempt(..., Failed/Canceled)` followed by
`FinishRequest`. If the host cannot prove this, keep the scope blocked. Recovery requires retained
private credentials for that scope; there is no administrative bypass or all-tenant inventory.
Missing managed state fails 409 `pacer_cache_state_missing`; it never implicitly enrolls.

## HTTP adapter and exact JSON fields

The host authenticates the exact inference-authenticated plugin path:

* `GET /v1/plugins/pacer/cache-policy`: scope query parameters `installation_id`, `session_id`,
  `provider_route`, plus `protocol=1`; protected capability header; call `Get`. No mutation.
* `POST /v1/plugins/pacer/cache-policy`: `application/json`, protected capability header;
  use `DecodeCommand` (16 KiB maximum, unknown fields/trailing JSON rejected), then `Apply`.

The host must check GET protocol, exact method/path allowlist, origin/HTTPS, authentication and
header protection. Those HTTP concerns are intentionally outside this package. No management key
is needed. Capabilities and caller identity are never accepted from query parameters or JSON.
Managed lifecycle validation remains mandatory when quota routing is disabled. Never convert a
policy error into an unmanaged request or silently fall back to the executor's default TTL.

`Command` JSON (scope fields are flattened):

```json
{
  "protocol": 1,
  "operation": "ensure",
  "installation_id": "installation-1",
  "session_id": "session-1",
  "provider_route": "claude-main",
  "context_epoch": "empty-epoch-1",
  "expected_revision": 0,
  "ttl": "5m",
  "operation_id": "unique-retry-stable-id",
  "enrollment": "new_empty"
}
```

Required common fields: `protocol` (integer 1), `operation` (`ensure|prepare|commit|cancel`),
the three scope strings, `context_epoch` string, `expected_revision` nonnegative int64,
`ttl` (`5m|1h`), `operation_id` string. IDs are 1–256 printable ASCII bytes without whitespace.
TTL is always explicit; the client defaults a new empty selector to 5m. Omission is not a downgrade.

Optional operation-specific fields (all rejected outside their documented operation):

| Field | Go / JSON type | Operation |
| --- | --- | --- |
| `enrollment` | string, only `new_empty` | initial `ensure`, with trusted `Evidence.NewEmpty` |
| `transition_id` | string | required for `commit` and `cancel` |
| `checkpoint` | string | required for `commit`; equals its new `context_epoch` |
| `compaction_started_at` | `time.Time`, RFC3339Nano | required for `commit` |
| `checkpoint_committed_at` | `time.Time`, RFC3339Nano | required for `commit` |

The two commit times are the authenticated client's durable Core checkpoint acknowledgement,
not proof of summary contents. They must establish **start strictly after `pending.prepared_at`**,
commit at/after start and no later than the server clock. Clients need a compatible clock basis;
clock skew fails closed. Merely requesting/attempting compaction is insufficient. A pre-prepare
checkpoint, unchanged checkpoint, or any previously used epoch is rejected. `generation`,
`caller_identity`, `skip_compaction`, and arbitrary enrollment modes are not protocol fields.

Responses are `State` directly:

```json
{
  "protocol": 1,
  "installation_id": "installation-1",
  "session_id": "session-1",
  "provider_route": "claude-main",
  "revision": 2,
  "active": {"context_epoch":"empty-epoch-1","checkpoint":"empty-epoch-1","ttl":"5m","used":true},
  "pending": {
    "transition_id":"server-random-transition-id",
    "ttl":"1h",
    "from_epoch":"empty-epoch-1",
    "from_checkpoint":"empty-epoch-1",
    "prepared_at":"2026-10-08T12:00:00Z",
    "prepared_revision":2
  },
  "active_primary":0,
  "active_warming":0,
  "active_auxiliary":0,
  "active_attempts":0,
  "warming_paused":true
}
```

`pending` is omitted when absent. `active_attempts` counts physical **managed primary/warming**
attempts; auxiliary attempts do not guard a primary TTL switch. Counts are int64. Revision starts
at 1 and increments on an empty-chat TTL edit, prepare, commit and cancel. It does not increment on
same-TTL ensure or lifecycle accounting. It is the coordination CAS revision, **not** forced-switch
generation. Prepare returns the revision required for commit/cancel. Active epoch/TTL/checkpoint
remain unchanged while pending; the active snapshots already issued keep their old revision.

Operation IDs are unique per caller across all scopes. Successful operations and authenticated
domain rejections are recorded durably. Identical decoded commands replay their original receipt
(which may be older than current GET state); changed payload reuse is 409 `operation_reused`.
Credentials are checked on replay. A receipt cannot replace missing durable state. A failed
operation (e.g. in-flight commit) needs a **new** operation ID after fixing the precondition.
Invalid/unauthorized input and storage failures are not journaled as completed operations.

## State transitions

1. Initial ensure: expected revision 0, `enrollment:new_empty`, trusted evidence; pins epoch/TTL.
   A previously registered session cannot open a second route as empty.
2. Before any primary/warming admission, same-epoch ensure may change TTL with the current CAS.
   After admission, direct TTL changes always require compaction—even after failed requests,
   account moves, cache expiry or restart. Same-TTL ensure is a no-op when no transition is pending.
3. Prepare: current epoch/CAS, target TTL. Keeps active policy, records a random transition and
   pauses new primary/warming requests **and physical retry attempts**. Existing requests drain.
   Same-TTL prepare is supported for ordinary/manual compaction into a new epoch.
4. Commit: new epoch == changed checkpoint, target TTL/transition/current CAS, fresh durable
   compaction acknowledgement, no old primary/warming logical requests or physical attempts.
   Atomically publishes policy and clears pending. The compacted epoch remains used history.
5. Cancel: old active epoch, pending target TTL/transition/current CAS. Clears pending, preserves
   active TTL/checkpoint/used. Existing requests keep their old snapshots; begin a fresh logical
   request if their revision no longer matches after cancellation.

A failed compaction sends cancel or leaves pending; failure never publishes a new TTL. Lost commit
responses are recovered by same-operation retry or GET. A forced-move compaction can satisfy a
pending transition only through this same fresh checkpoint acknowledgement; generations do nothing.

## Lifecycle structs and host sequence

```go
Reference{Scope Scope, ContextEpoch string, Revision int64, TTL TTL} // Scope embedded
Request{Reference Reference, RequestID string, Kind Kind}          // Reference embedded
Attempt{Scope Scope, RequestID string, AttemptID string}           // Scope embedded
// Kind = Primary ("primary"), Warming ("warming"), Auxiliary ("auxiliary")
// Outcome = Succeeded ("succeeded"), Failed ("failed"), Canceled ("canceled")
Snapshot{
    Protocol int, Scope Scope, ContextEpoch string, Revision int64, TTL TTL,
    RequestID string, AttemptID string, Kind Kind, Managed bool, StartedAt time.Time,
} // Scope embedded, JSON snake_case; ttl/attempt_id omitted when empty
```

1. The host classifies request kind; a summary/title is auxiliary, never primary or warming.
   `BeginRequest` validates the full policy reference and durably marks primary/warming used.
2. For every physical upstream dispatch/retry, call `BeginAttempt` with a **new physical attempt
   ID** and the same lifecycle request ID. This returns its durable immutable snapshot, with the
   physical request-start time. Pin it through translation, final executor policy and usage.
3. `ValidateAttempt` checks every snapshot field against its active durable receipt. It permits
   an already-started attempt to finish under its old snapshot while prepare is pending.
   The executor separately validates its final body/headers. Never redispatch the same attempt ID.
4. Settle each physical attempt, then its logical request, including errors/cancellation/streams.
   Same terminal outcome is idempotent; a different terminal outcome is 409. Do not settle merely
   because a client disappeared. A terminal attempt/request cannot be reopened with the same ID.
5. Correlate late/deduplicated usage using `GetAttempt`, even after a later epoch is active.
   It returns selected policy, not provider-reported billing or final effective wire policy.

Duplicate *active* lifecycle begins return the same immutable snapshot without incrementing
counts; these are lost-receipt recovery calls, **not authorization to dispatch the same attempt
twice**. The host owns physical dispatch deduplication. Snapshot values are independent copies.
Auxiliary snapshots carry `managed:false` and omit TTL, remain allowed while pending, never mark
primary history used/warm and never block primary commit. This package does not schedule warming.

## Errors and limitations

Errors are `*cachepolicy.Error{Status int, Code string, Message string}`; serialize as
`{"error":{"code":"pacer_cache_...","message":"..."}}` with `Status` as HTTP status.
Codes: 400 `invalid`; 403 `scope_denied`; 422 `unsupported`, `enrollment_unsupported`; 503
`unavailable`; 409 `state_missing`, `operation_reused`, `revision_conflict`, `epoch_conflict`,
`policy_conflict`, `transition_pending`, `transition_conflict`, `compaction_required`,
`checkpoint_required`, `inflight`, `request_reused`, `request_missing`, `request_settled`,
`attempt_reused`, `attempt_missing`, `attempt_settled`, `snapshot_conflict`, `outcome_conflict`.
Foreign missing scopes reveal no policy and return generic state-missing; an existing scope with
wrong capability returns scope-denied. Database internals/credentials are never in wire errors.

No executor/HTTP/client integration, effective-wire or usage telemetry, compaction controller,
enrollment attestation issuer, fork inheritance, key rotation, retention/garbage collection,
automatic orphan cleanup, TTL-based warmth inference or provider calls are implemented here.
The operation and lifecycle journals intentionally retain receipts; pruning requires a separate
versioned replay-retention design. Timestamps rely on honest authenticated checkpoint acknowledgement.
