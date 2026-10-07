# Cache policy protocol 1, contract revision 2 — internal integration contract

Implemented by `internal/cachepolicy`, independently of routing/account-switch generation.
No HTTP handler, executor rewrite, client controller, quota reservation or adaptive policy lives here.
`cachepolicy.Protocol == 1`, `cachepolicy.ContractRevision == 2`, and
`cachepolicy.CheckpointReceiptVersion == 1`. See the compatibility section before integration.

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
by the client **before initial ensure or enrollment prepare**, so lost responses can be retried. Only its SHA-256 hash is
persisted. It is never in JSON responses, URLs, prompts or logs. Missing/invalid capability fails.
Neither credentials nor `Evidence` may be decoded from client operation JSON.

`Evidence.NewEmpty` records the host's acceptance of an **authenticated trusted client-plugin
attestation** that its local Core session is genuinely empty. The OCP plugin checks its local
history before declaring `enrollment:new_empty`; CPA need not query or inspect its private DB.
This is not inferred from a missing server policy, a new session ID, a generation, or an arbitrary
untrusted body field. Without a trusted empty-session attestation, `ensure` enrollment returns 422.

Existing, unknown or copied history instead declares `enrollment:existing_history` in an initial
`prepare`. It needs no new-empty attestation: a scoped, capability-bound enrollment guard is created
with **unknown active TTL**, and no managed primary/warming request is admitted until a fresh
compaction commits. A copied child uses its own owned session scope and capability. This supports
enrollment after compaction, not direct fork inheritance without compaction. Route migration for
an already registered session and ownership transfer remain unsupported. Existing use never becomes
empty again, including after canceling enrollment.

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
| `enrollment` | string `new_empty` or `existing_history` | `new_empty` only with `ensure` plus `Evidence.NewEmpty`; `existing_history` only with enrollment `prepare` |
| `transition_id` | string | required for `commit` and `cancel` |
| `checkpoint` | string | required for `commit`; equals its new `context_epoch` |
| `checkpoint_receipt` | `*CheckpointReceipt`, object below | required for every **new** `commit` |
| `compaction_started_at` | `time.Time`, RFC3339Nano | optional client provenance, only for `commit` |
| `checkpoint_committed_at` | `time.Time`, RFC3339Nano | optional client provenance, only for `commit` |

Exact receipt type (all five fields required):

```go
type CheckpointReceipt struct {
    Version            int    `json:"version"`             // exactly 1
    TransitionID       string `json:"transition_id"`       // pending.transition_id
    PreviousCheckpoint string `json:"previous_checkpoint"` // pending.from_checkpoint
    Checkpoint         string `json:"checkpoint"`          // commit.checkpoint
    Committed          bool   `json:"committed"`           // exactly true
}
```

The receipt is the authenticated client's durable Core acknowledgement, **not cryptographic
proof of summary contents**. Its meaning is: after receiving this prepare transition, the client
bound that transition to a new compaction of the specified previous checkpoint, then Core durably
committed the specified new checkpoint. Merely echoing the token while requesting/attempting
compaction is not sufficient. The client must not attach a new transition to a checkpoint created
before prepare. A receipt for another/canceled transition, a different previous checkpoint, an
uncommitted checkpoint, an unchanged checkpoint, or a previously used epoch is rejected.

**Client ordering/recovery:** durably retain scope, capability, transition ID, prepared revision
and previous checkpoint before starting compaction. Associate this tuple with the Core compaction
operation. Only after Core durably commits a new checkpoint, persist the completed receipt and the
retry-stable commit operation ID/payload before sending commit. If the client crashes between Core
commit and receipt persistence, recover using durable Core compaction/checkpoint correlation; if
that correlation cannot establish this transition's completion, reconcile/cancel and compact anew.
Never infer completion from timestamps, a compaction-start event, account move, or elapsed time.

`compaction_started_at`, `checkpoint_committed_at`, and server `pending.prepared_at` are provenance,
**not an ordering proof**. There is no client/server wall-clock comparison and no requirement that
client timestamps increase (wall clocks can step backward). Future +1s, earlier dates and omitted
times do not reject an otherwise valid receipt. Zero `time.Time` means unavailable; the Go encoder
may emit its zero timestamp even with `omitempty`. This does not claim that a real compaction can
finish before it starts; the receipt asserts logical order independently of wall-clock readings.
`generation`, `caller_identity` and `skip_compaction` are not protocol fields.

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

Two additive `State` fields:

* `enrollment_required` (Go `bool`, omitted when false): when true, `active` describes only the old
  context anchor, with `ttl:""` (unknown) and `used:true`. It is **not an active managed policy**.
  Primary/warming admission and direct ensure fail `409 pacer_cache_enrollment_required`.
  `warming_paused` stays true even after enrollment is canceled. Auxiliary summaries may run with
  `Reference.TTL == ""`; their snapshots remain unmanaged and carry no selected TTL.
* `last_commit` (Go `*CommitProvenance`, omitted before a receipt-based commit): retained provenance
  for the latest active checkpoint. Its exact shape is below. Earlier committed receipts remain
  in operation results. Historical state predating this extension may lack it.

```go
type CommitProvenance struct {
    Receipt               CheckpointReceipt `json:"checkpoint_receipt"`
    CompactionStartedAt   time.Time         `json:"compaction_started_at,omitempty"`
    CheckpointCommittedAt time.Time         `json:"checkpoint_committed_at,omitempty"`
    AcknowledgedAt        time.Time         `json:"acknowledged_at"` // server clock
}
```

Operation IDs are unique per caller across all scopes. Successful operations and authenticated
domain rejections are recorded durably. Identical decoded commands replay their original receipt
(which may be older than current GET state); changed payload reuse is 409 `operation_reused`.
Credentials are checked on replay. A receipt cannot replace missing durable state. A failed
operation (e.g. in-flight commit) needs a **new** operation ID after fixing the precondition.
Invalid/unauthorized input and storage failures are not journaled as completed operations.

## State transitions

1. Initial empty ensure: expected revision 0, `enrollment:new_empty`, trusted evidence; pins epoch/TTL.
   A previously registered session cannot open a second route as empty.
2. Before any primary/warming admission, same-epoch ensure may change TTL with the current CAS.
   After admission, direct TTL changes always require compaction—even after failed requests,
   account moves, cache expiry or restart. Same-TTL ensure is a no-op when no transition is pending.
3. Prepare: current epoch/CAS, target TTL. Keeps active policy, records a random transition and
   pauses new primary/warming requests **and physical retry attempts**. Existing requests drain.
   Same-TTL prepare is supported for ordinary/manual compaction into a new epoch.
4. Commit: new epoch == changed checkpoint, target TTL/transition/current CAS, fresh durable
   transition-bound checkpoint receipt, no old primary/warming logical requests or physical attempts.
   Atomically publishes policy and clears pending. The compacted epoch remains used history.
5. Cancel: old active epoch, pending target TTL/transition/current CAS. Clears pending, preserves
   active TTL/checkpoint/used. Existing requests keep their old snapshots; begin a fresh logical
   request if their revision no longer matches after cancellation.

A failed compaction sends cancel or leaves pending; failure never publishes a new TTL. Lost commit
responses are recovered by same-operation retry or GET. A forced-move compaction can satisfy a
pending transition only through this same fresh checkpoint acknowledgement; generations do nothing.

### Initial existing/unknown/copied-history enrollment

The client pauses/drains its old primary work before enrolling. Its existing legacy requests may
not yet be tracked by this store; the client owns this local safe boundary. Once the scope guard
exists, host integration must honor it and must not fall back to unmanaged dispatch on refusal.

```json
{
  "protocol":1,
  "operation":"prepare",
  "installation_id":"installation-1",
  "session_id":"existing-session",
  "provider_route":"claude-main",
  "context_epoch":"old-core-checkpoint",
  "expected_revision":0,
  "ttl":"1h",
  "operation_id":"enroll-prepare-1",
  "enrollment":"existing_history"
}
```

The current committed Core checkpoint is the baseline `context_epoch`. If the chat has history
but no checkpoint yet, the client persists a stable opaque context-anchor ID for that history
(for example `uncompacted:<session-id>`) and uses it consistently as the baseline. This is never
an empty-history claim, a forced-switch generation, or evidence of a prior compaction.

The initial prepare returns revision **1**, `enrollment_required:true`, `warming_paused:true`,
`active:{context_epoch:"old-core-checkpoint",checkpoint:"old-core-checkpoint",ttl:"",used:true}`,
and a pending transition targeting `1h`. Preserve the returned scope/capability/revision/transition.
Do not send primary context under the pending target.

After a new durable Core compaction, send (the returned transition ID is substituted twice):

```json
{
  "protocol":1,
  "operation":"commit",
  "installation_id":"installation-1",
  "session_id":"existing-session",
  "provider_route":"claude-main",
  "context_epoch":"new-core-checkpoint",
  "expected_revision":1,
  "ttl":"1h",
  "operation_id":"enroll-commit-1",
  "transition_id":"returned-transition-id",
  "checkpoint":"new-core-checkpoint",
  "checkpoint_receipt":{
    "version":1,
    "transition_id":"returned-transition-id",
    "previous_checkpoint":"old-core-checkpoint",
    "checkpoint":"new-core-checkpoint",
    "committed":true
  }
}
```

This exact receipt contract also applies to TTL switches and same-TTL/manual compaction. Successful
initial commit returns revision **2**, clears enrollment/pending, publishes the selected TTL and
new checkpoint, and keeps `active.used:true`. Missing/invalid receipts never pin an initial TTL.

On failed/canceled initial compaction, leave pending or send normal cancel using the old anchor,
pending target TTL, transition ID and revision. Cancel retains the enrollment guard/capability,
unknown TTL and used history; there is no active policy to revert to and warming remains paused.
To retry, send prepare with the current GET revision and unchanged baseline anchor, with either
`enrollment:existing_history` or omitted enrollment. It returns a **new** transition. An old receipt
cannot satisfy it. A later `new_empty` claim cannot convert this guard into a fresh empty chat.

An initial prepare with expected revision >0, or one targeting a scope whose retained receipts
show lost enrollment state, fails `state_missing`. No managed request enrolls automatically.

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

## Compatibility with the first protocol-1 implementation

Host capability discovery must advertise/use `cachepolicy.ContractRevision` **2** before enabling
this enrollment/receipt flow. No new endpoint, authentication mode, Go method signature or SQLite
table is introduced. State JSON extensions are additive; schema version remains 1. `Open` continues
to read existing active states and pending transitions without rewriting them, initializing missing
state, or needing access to client DBs. An upgraded client can bind a proper receipt to a retained
old pending transition before undertaking a new compaction.

* Legacy `ensure`/prepare/cancel and active lifecycle wire fields remain valid. New receipt fields
  are omitted from old command fingerprints, preserving historical operation-ID replay.
* **New timestamp-only commits now fail 422 `pacer_cache_checkpoint_receipt_required`.** There is
  deliberately no timestamp fallback. A new client receipt after this rejection uses a new
  operation ID, since the old ID's payload/rejection is durable.
* An identical retry of a **previously successful** timestamp-only commit still returns its old
  durable result after restart/upgrade, without performing another commit or manufacturing a new
  receipt. The replay still requires matching caller/scope/capability and present correctness state.
* An older server's strict decoder rejects `checkpoint_receipt` and `existing_history` enrollment;
  clients must not hide that error or retry as `new_empty`/an unmanaged TTL. An older reader sees an
  enrollment guard's empty TTL, not an invented 5m policy. Do not downgrade the host during pending
  enrollment; use the compatible executor/store build and explicit reconciliation.

## Errors and limitations

Errors are `*cachepolicy.Error{Status int, Code string, Message string}`; serialize as
`{"error":{"code":"pacer_cache_...","message":"..."}}` with `Status` as HTTP status.
Codes: 400 `invalid`; 403 `scope_denied`; 422 `unsupported`, `enrollment_unsupported`,
`checkpoint_receipt_required`; 503
`unavailable`; 409 `state_missing`, `operation_reused`, `revision_conflict`, `epoch_conflict`,
`policy_conflict`, `transition_pending`, `transition_conflict`, `compaction_required`,
`checkpoint_required`, `enrollment_required`, `inflight`, `request_reused`, `request_missing`, `request_settled`,
`attempt_reused`, `attempt_missing`, `attempt_settled`, `snapshot_conflict`, `outcome_conflict`.
Foreign missing scopes reveal no policy and return generic state-missing; an existing scope with
wrong capability returns scope-denied. Database internals/credentials are never in wire errors.

No executor/HTTP/client integration, effective-wire or usage telemetry, compaction controller,
client-side attestation/receipt persistence, direct fork inheritance without compaction, route
migration, key rotation, retention/garbage collection,
automatic orphan cleanup, TTL-based warmth inference or provider calls are implemented here.
The operation and lifecycle journals intentionally retain receipts; pruning requires a separate
versioned replay-retention design. The receipt relies on honest authenticated checkpoint
acknowledgement; it cannot detect a client deliberately lying about Core contents or logical order.
