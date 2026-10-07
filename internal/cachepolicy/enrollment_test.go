package cachepolicy

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func (f *fixture) existing() Command {
	return Command{Protocol: Protocol, Operation: "prepare", Scope: f.scope, ContextEpoch: "old-core-checkpoint", ExpectedRevision: 0, TTL: TTL1h, OperationID: f.id(), Enrollment: "existing_history"}
}

func (f *fixture) reopen() {
	f.t.Helper()
	if err := f.db.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.db = openDB(f.t, f.path)
	var err error
	f.s, err = Open(ctx, f.db, f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
}

func requireEnrollmentBlocked(t *testing.T, f *fixture, state State) {
	t.Helper()
	if !state.EnrollmentRequired || !state.WarmingPaused || state.Active.TTL != "" || !state.Active.Used || state.LastCommit != nil {
		t.Fatalf("existing history acquired a policy before compaction: %+v", state)
	}
	for _, kind := range []Kind{Primary, Warming} {
		_, err := f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "blocked-" + string(kind), Kind: kind})
		requireCode(t, err, 409, "enrollment_required")
	}
}

func TestExistingHistoryEnrollsOnlyAfterCompactionReceipt(t *testing.T) {
	f := newFixture(t)
	cmd := f.existing()
	pending, err := f.s.Apply(ctx, f.auth, cmd, Evidence{}) // no new-empty attestation
	if err != nil {
		t.Fatal(err)
	}
	requireEnrollmentBlocked(t, f, pending)
	if pending.Revision != 1 || pending.Pending == nil || pending.Pending.FromCheckpoint != cmd.ContextEpoch || pending.Pending.TTL != TTL1h {
		t.Fatal(pending)
	}
	fresh := f.command("ensure", pending, TTL5m)
	fresh.Enrollment = "new_empty"
	_, err = f.s.Apply(ctx, f.auth, fresh, Evidence{NewEmpty: true})
	requireCode(t, err, 409, "enrollment_required")
	// Summary work is auxiliary and has no selected TTL, even during enrollment.
	if snap := f.request(pending, "summary", Auxiliary); snap.Managed || snap.TTL != "" {
		t.Fatal(snap)
	}
	a, aux := f.attempt("summary", "summary-attempt")
	if err = f.s.ValidateAttempt(ctx, f.auth, aux); err != nil {
		t.Fatal(err)
	}
	commit := f.commit(pending, "new-core-checkpoint")
	active := f.apply(commit)
	if active.EnrollmentRequired || active.WarmingPaused || active.Pending != nil || active.Active.TTL != TTL1h || !active.Active.Used || active.Revision != 2 || active.LastCommit == nil {
		t.Fatal(active)
	}
	if !reflect.DeepEqual(active.LastCommit.Receipt, *commit.CheckpointReceipt) {
		t.Fatal("receipt provenance was not retained")
	}
	f.settle(a, Succeeded)
	f.request(active, "first-managed-primary", Primary)
	_, err = f.s.Apply(ctx, f.auth, f.command("ensure", active, TTL5m), Evidence{NewEmpty: true})
	requireCode(t, err, 409, "compaction_required")
}

func TestFailedEnrollmentCompactionCancelAndRestartNeverBecomeFresh(t *testing.T) {
	f := newFixture(t)
	prepare := f.existing()
	pending, err := f.s.Apply(ctx, f.auth, prepare, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if got := f.apply(prepare); !reflect.DeepEqual(got, pending) {
		t.Fatal("initial prepare receipt changed on restart")
	}
	requireEnrollmentBlocked(t, f, f.get())
	f.request(pending, "failed-summary", Auxiliary)
	a, _ := f.attempt("failed-summary", "failed-attempt")
	f.settle(a, Failed)
	failed := f.commit(pending, "uncommitted-checkpoint")
	failed.CheckpointReceipt.Committed = false
	_, err = f.s.Apply(ctx, f.auth, failed, Evidence{})
	requireCode(t, err, 409, "checkpoint_required")
	requireEnrollmentBlocked(t, f, f.get())
	old := f.commit(pending, pending.Active.Checkpoint)
	_, err = f.s.Apply(ctx, f.auth, old, Evidence{})
	requireCode(t, err, 409, "checkpoint_required")
	cancel := f.command("cancel", pending, pending.Pending.TTL)
	cancel.TransitionID = pending.Pending.ID
	canceled := f.apply(cancel)
	if canceled.Pending != nil || canceled.Revision != 2 {
		t.Fatal(canceled)
	}
	f.reopen()
	requireEnrollmentBlocked(t, f, f.get())
	fresh := f.command("ensure", canceled, TTL5m)
	fresh.Enrollment = "new_empty"
	_, err = f.s.Apply(ctx, f.auth, fresh, Evidence{NewEmpty: true})
	requireCode(t, err, 409, "enrollment_required")
	// Reprepare uses the retained scope, capability, checkpoint and current CAS.
	again := f.command("prepare", canceled, TTL5m)
	again.Enrollment = "existing_history"
	retry := f.apply(again)
	if retry.Pending.ID == pending.Pending.ID || retry.Active != pending.Active {
		t.Fatal("reprepare reused old transition or reclassified history")
	}
	oldReceipt := f.commit(retry, "new-checkpoint")
	oldReceipt.CheckpointReceipt.TransitionID = pending.Pending.ID
	_, err = f.s.Apply(ctx, f.auth, oldReceipt, Evidence{})
	requireCode(t, err, 409, "checkpoint_required")
	commit := f.commit(retry, "new-checkpoint")
	active := f.apply(commit)
	f.reopen()
	if got := f.apply(commit); !reflect.DeepEqual(got, active) {
		t.Fatal("lost commit acknowledgement changed after restart")
	}
	if got := f.apply(prepare); !reflect.DeepEqual(got, pending) {
		t.Fatal("original prepare idempotency receipt lost")
	}
	if got := f.get(); !reflect.DeepEqual(got, active) {
		t.Fatal("old operation replay changed current state")
	}
	changed := commit
	receipt := *commit.CheckpointReceipt
	receipt.PreviousCheckpoint = "other"
	changed.CheckpointReceipt = &receipt
	_, err = f.s.Apply(ctx, f.auth, changed, Evidence{})
	requireCode(t, err, 409, "operation_reused")
}

func TestCopiedHistoryAndCrossScopeReceipts(t *testing.T) {
	f := newFixture(t)
	initial := f.initial()
	initial.TTL = TTL1h
	parent := f.apply(initial)
	f.request(parent, "parent-used", Primary)
	if err := f.s.FinishRequest(ctx, f.auth, f.scope, "parent-used", Succeeded); err != nil {
		t.Fatal(err)
	}
	parent = f.get()
	childScope := f.scope
	childScope.SessionID = "copied-child"
	childAuth := f.auth
	childAuth.Capability = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	fresh := f.initial()
	fresh.Scope = childScope
	fresh.ContextEpoch = parent.Active.Checkpoint
	_, err := f.s.Apply(ctx, childAuth, fresh, Evidence{})
	requireCode(t, err, 422, "enrollment_unsupported")
	prepare := f.existing()
	prepare.Scope = childScope
	prepare.ContextEpoch = parent.Active.Checkpoint
	prepare.TTL = TTL5m
	child, err := f.s.Apply(ctx, childAuth, prepare, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if !child.EnrollmentRequired || child.Active.TTL != "" {
		t.Fatal("copied history silently downgraded to a fresh policy")
	}
	_, err = f.s.BeginRequest(ctx, childAuth, Request{Reference: ref(child), RequestID: "copied-primary", Kind: Primary})
	requireCode(t, err, 409, "enrollment_required")
	_, err = f.s.Get(ctx, f.auth, childScope)
	requireCode(t, err, 403, "scope_denied")
	commit := f.commit(child, "child-compaction")
	commit.Scope = childScope
	_, err = f.s.Apply(ctx, f.auth, commit, Evidence{})
	requireCode(t, err, 403, "scope_denied")
	// A receipt from another owned scope still cannot authorize this transition.
	parentPending := f.apply(f.command("prepare", parent, TTL1h))
	foreign := f.commit(parentPending, commit.Checkpoint)
	bad := commit
	bad.CheckpointReceipt = foreign.CheckpointReceipt
	_, err = f.s.Apply(ctx, childAuth, bad, Evidence{})
	requireCode(t, err, 409, "checkpoint_required")
	commit.OperationID = f.id()
	active, err := f.s.Apply(ctx, childAuth, commit, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if active.EnrollmentRequired || active.Active.TTL != TTL5m || !active.Active.Used {
		t.Fatal(active)
	}
	if got := f.get(); !reflect.DeepEqual(got, parentPending) {
		t.Fatal("child enrollment changed parent policy")
	}
	// Wrong installation/route/caller cannot consume or replay the child commit.
	for _, scope := range []Scope{
		{InstallationID: "foreign-install", SessionID: childScope.SessionID, ProviderRoute: childScope.ProviderRoute},
		{InstallationID: childScope.InstallationID, SessionID: childScope.SessionID, ProviderRoute: "five-only"},
	} {
		wrong := commit
		wrong.Scope = scope
		wrong.OperationID = f.id()
		_, err = f.s.Apply(ctx, childAuth, wrong, Evidence{})
		requireCode(t, err, 409, "state_missing")
	}
	foreignAuth := childAuth
	foreignAuth.CallerDigest = hashString("another-caller")
	_, err = f.s.Apply(ctx, foreignAuth, commit, Evidence{})
	requireCode(t, err, 409, "state_missing")
}

func TestCheckpointReceiptAllowsClockSkewAndRetainsProvenance(t *testing.T) {
	for _, enrollment := range []bool{false, true} {
		for _, timing := range []string{"ahead-one-second", "behind-one-day", "clock-stepped-back", "absent"} {
			t.Run(map[bool]string{true: "enrollment", false: "switch"}[enrollment]+"/"+timing, func(t *testing.T) {
				f := newFixture(t)
				var pending State
				if enrollment {
					pending = f.apply(f.existing())
				} else {
					pending = f.apply(f.command("prepare", f.apply(f.initial()), TTL1h))
				}
				commit := f.commit(pending, "new-checkpoint")
				switch timing {
				case "ahead-one-second":
					commit.CompactionStartedAt = f.clock.now().Add(time.Second)
					commit.CheckpointCommittedAt = commit.CompactionStartedAt.Add(time.Second)
				case "behind-one-day":
					commit.CompactionStartedAt = pending.Pending.PreparedAt.Add(-24 * time.Hour)
					commit.CheckpointCommittedAt = commit.CompactionStartedAt.Add(time.Second)
				case "clock-stepped-back":
					commit.CheckpointCommittedAt = commit.CompactionStartedAt.Add(-time.Second)
				case "absent":
					commit.CompactionStartedAt, commit.CheckpointCommittedAt = time.Time{}, time.Time{}
				}
				active := f.apply(commit)
				p := active.LastCommit
				if p == nil || !p.CompactionStartedAt.Equal(commit.CompactionStartedAt) || !p.CheckpointCommittedAt.Equal(commit.CheckpointCommittedAt) || !p.AcknowledgedAt.Equal(f.clock.now()) {
					t.Fatalf("timestamps lost or fabricated: %+v", p)
				}
				// Caller mutation cannot rewrite the saved receipt.
				commit.CheckpointReceipt.Checkpoint = "mutated"
				if got := f.get(); !reflect.DeepEqual(got, active) {
					t.Fatal("receipt alias mutated durable policy")
				}
			})
		}
	}
}

func TestLegacyProtocolOneRequiresReceiptForNewCommit(t *testing.T) {
	f := newFixture(t)
	const legacyEnsure = `{"protocol":1,"operation":"ensure","installation_id":"install-A","session_id":"session-A","provider_route":"claude-main","context_epoch":"empty-epoch","expected_revision":0,"ttl":"5m","operation_id":"legacy-ensure","compaction_started_at":"0001-01-01T00:00:00Z","checkpoint_committed_at":"0001-01-01T00:00:00Z","enrollment":"new_empty"}`
	cmd, err := DecodeCommand(strings.NewReader(legacyEnsure))
	if err != nil {
		t.Fatal(err)
	}
	if digest(cmd) != hashString(legacyEnsure) {
		t.Fatal("additive fields changed old operation fingerprint")
	}
	initial := f.apply(cmd)
	pending := f.apply(f.command("prepare", initial, TTL1h))
	legacyCommit := f.commit(pending, "new-checkpoint")
	legacyCommit.CheckpointReceipt = nil
	_, err = f.s.Apply(ctx, f.auth, legacyCommit, Evidence{})
	requireCode(t, err, 422, "checkpoint_receipt_required")
	if got := f.get(); !reflect.DeepEqual(got, pending) {
		t.Fatal("timestamp-only commit altered state")
	}
	upgraded := f.commit(pending, "new-checkpoint")
	upgraded.OperationID = legacyCommit.OperationID
	_, err = f.s.Apply(ctx, f.auth, upgraded, Evidence{})
	requireCode(t, err, 409, "operation_reused")
	upgraded.OperationID = f.id()
	f.apply(upgraded)
	// Wire extensions are exact; unknown receipt fields/version do not downgrade.
	raw, err := json.Marshal(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCommand(strings.NewReader(string(raw)))
	if err != nil || !reflect.DeepEqual(decoded, upgraded) {
		t.Fatal(decoded, err)
	}
	unknown := strings.Replace(string(raw), `"committed":true`, `"committed":true,"skip_compaction":true`, 1)
	_, err = DecodeCommand(strings.NewReader(unknown))
	requireCode(t, err, 400, "invalid")
	pending = f.apply(f.command("prepare", f.get(), TTL5m))
	badVersion := f.commit(pending, "another-checkpoint")
	badVersion.CheckpointReceipt.Version = 2
	_, err = f.s.Apply(ctx, f.auth, badVersion, Evidence{})
	requireCode(t, err, 422, "unsupported")
}

func TestLegacyCompletedOperationAndStoreReopenCompatibility(t *testing.T) {
	f := newFixture(t)
	f.apply(f.initial())
	// Frozen protocol-1 bytes model an already-committed operation from 77edab6.
	const commandJSON = `{"protocol":1,"operation":"commit","installation_id":"install-A","session_id":"session-A","provider_route":"claude-main","context_epoch":"legacy-committed","expected_revision":2,"ttl":"1h","operation_id":"legacy-commit","transition_id":"legacy-transition","checkpoint":"legacy-committed","compaction_started_at":"2026-10-08T12:00:01Z","checkpoint_committed_at":"2026-10-08T12:00:02Z"}`
	const stateJSON = `{"protocol":1,"installation_id":"install-A","session_id":"session-A","provider_route":"claude-main","revision":3,"active":{"context_epoch":"legacy-committed","checkpoint":"legacy-committed","ttl":"1h","used":true},"active_primary":0,"active_warming":0,"active_auxiliary":0,"active_attempts":0,"warming_paused":false}`
	cmd, err := DecodeCommand(strings.NewReader(commandJSON))
	if err != nil {
		t.Fatal(err)
	}
	if digest(cmd) != hashString(commandJSON) {
		t.Fatal("legacy commit fingerprint changed")
	}
	var old State
	if err = json.Unmarshal([]byte(stateJSON), &old); err != nil {
		t.Fatal(err)
	}
	key, capHash, err := credentials(f.auth, f.scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`UPDATE cache_policy_scopes SET state=? WHERE scope_key=?`, []byte(stateJSON), key); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`INSERT INTO cache_policy_epochs(scope_key,context_epoch) VALUES(?,?)`, key, old.Active.ContextEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`INSERT INTO cache_policy_operations(caller_digest,operation_id,payload_hash,scope_key,capability_hash,result) VALUES(?,?,?,?,?,?)`, f.auth.CallerDigest, cmd.OperationID, hashString(commandJSON), key, capHash, []byte(stateJSON)); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if got := f.apply(cmd); !reflect.DeepEqual(got, old) {
		t.Fatal("legacy successful receipt cannot recover after upgrade")
	}
	var persisted []byte
	if err = f.db.QueryRow(`SELECT state FROM cache_policy_scopes WHERE scope_key=?`, key).Scan(&persisted); err != nil || string(persisted) != stateJSON {
		t.Fatal("Open/replay rewrote legacy state", err)
	}
	pending := f.apply(f.command("prepare", old, TTL5m))
	active := f.apply(f.commit(pending, "receipt-bound-checkpoint"))
	if active.LastCommit == nil || active.Active.TTL != TTL5m {
		t.Fatal(active)
	}
}

func TestExistingEnrollmentCannotRepairMissingCorrectnessState(t *testing.T) {
	f := newFixture(t)
	prepare := f.existing()
	f.apply(prepare)
	if _, err := f.db.Exec(`DELETE FROM cache_policy_scopes`); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Apply(ctx, f.auth, prepare, Evidence{})
	requireCode(t, err, 409, "state_missing")
	newID := f.existing()
	_, err = f.s.Apply(ctx, f.auth, newID, Evidence{})
	requireCode(t, err, 409, "state_missing")
	if _, err = f.db.Exec(`DELETE FROM cache_policy_meta`); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, f.db, f.opts)
	requireCode(t, err, 503, "unavailable")
}
