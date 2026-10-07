# Existing-history enrollment implementation receipt

2026-10-08, isolated `pacer-policy` worktree. Follow-up to
`77edab6712db48cfc53993dce5634646be03ff09` (parent cherry-pick reported as `6f50965`).

## Delivered

- Protocol 1 **contract revision 2**, checkpoint receipt version 1; exact integration contract:
  [cache-policy-contract.md](cache-policy-contract.md).
- Initial `prepare` with `enrollment:"existing_history"`, expected revision 0 and an explicit old
  context/checkpoint anchor creates a durable, capability-bound enrollment guard. It does not
  infer a selected TTL or treat missing state as fresh. Existing/unknown/copied history can enroll
  after a new committed compaction; primary/warming are blocked until then.
- Cancel preserves unknown TTL, used history, caller/capability ownership and the enrollment guard.
  A new-empty claim cannot bypass it. Reprepare uses the current CAS and a new transition token.
- Every new commit requires `checkpoint_receipt:{version:1,transition_id,previous_checkpoint,
  checkpoint,committed:true}`, matching the scoped pending transition and changed checkpoint.
  Reused epochs, old/mismatched transition receipts and uncommitted checkpoints fail closed.
- Client timestamps are optional provenance, retained in `State.LastCommit` with a server
  acknowledgement timestamp. No wall-clock ordering comparison or synchronized clocks is required.
- `Evidence.NewEmpty` may reflect a trusted authenticated OCP plugin's local empty-session
  attestation; no CPA query of private client DBs is required or implemented.
- Existing schema/data and operation fingerprints remain readable. Previously successful legacy
  commits can recover their old durable response; new timestamp-only commits return 422
  `pacer_cache_checkpoint_receipt_required`. No new tables or Go method signatures were introduced.

## Verification

Passed on local Go 1.27.1 darwin/arm64 with real file-backed SQLite (`go-sqlite3 v1.14.32`):

```text
go test -count=1 -v ./internal/cachepolicy
go test -race -count=1 -coverprofile=.cache/enrollment-coverage.out -v ./internal/cachepolicy
go vet ./internal/cachepolicy
git diff --check
```

20 top-level tests and 14 subtests passed; race run: **81.5% statement coverage**, no reported races.
Local raw evidence is retained in this worktree's ignored `.cache/enrollment-race-tests.txt` and
`.cache/enrollment-coverage.out`. The first focused follow-up run passed; no failing verification
is being omitted from this follow-up receipt.

New coverage includes initial existing-history enrollment; missing/new-empty refusal; auxiliary
summary isolation; failed summary and uncommitted checkpoint refusal; cancellation/reprepare;
old checkpoint/old receipt refusal; restart and lost prepare/commit responses; receipt-payload
conflict; copied-history child scope and foreign caller/capability/installation/route; +1s and
negative clock skew, a clock stepping backward and absent timestamps; legacy operation fingerprint,
legacy completed receipt and unchanged store reopen compatibility; missing state/meta rejection.
The prior CAS, concurrent multi-handle SQLite, immutable attempt, in-flight and injected rollback
tests continue to pass.

## Remaining integration boundaries

The authenticated client's committed Core acknowledgement is the authority; this is not proof of
summary contents. The client must persist transition/old-checkpoint association before compaction
and recover a committed receipt only from durable Core correlation. Attaching a new transition to
a checkpoint created before prepare violates the contract. Host discovery should expose contract
revision 2 before the client enables this flow. The implementation does not supply client Core hooks,
HTTP/executor integration, physical dispatch deduplication or provider calls.

Copied children are supported through their own compaction-gated enrollment; direct no-compaction
fork inheritance, registered-session route migration and ownership/key transfer remain unsupported.
Initial enrollment relies on the client draining pre-enrollment primary work, which may not have
been visible to this store. Once the guard exists, host integration must not bypass it as unmanaged.
`Create` remains explicit first initialization; `Open` never initializes missing correctness state.

All source/docs edits are confined to this policy worktree. No parent source/evidence, runtime,
production, remote or external-Mac files were changed. Previously authorized local Go/module/temp
caches remain under `pacer/.cache/policy`; commands used `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`.
