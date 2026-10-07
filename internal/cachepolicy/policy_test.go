package cachepolicy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ctx = context.Background()

type testClock struct{ nanos atomic.Int64 }

func newClock() *testClock {
	c := &testClock{}
	c.nanos.Store(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *testClock) now() time.Time     { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *testClock) advance() time.Time { return time.Unix(0, c.nanos.Add(int64(time.Second))).UTC() }

type fixture struct {
	t     *testing.T
	db    *sql.DB
	s     *Store
	path  string
	clock *testClock
	auth  Credentials
	scope Scope
	opts  Options
	seq   int
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=10000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	return db
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := newClock()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "policy.sqlite"), clock: c,
		auth:  Credentials{CallerDigest: hashString("host caller A"), Capability: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))},
		scope: Scope{InstallationID: "install-A", SessionID: "session-A", ProviderRoute: "claude-main"},
		opts:  Options{Clock: c.now, Routes: []Route{{ProviderRoute: "claude-main", TTLs: []TTL{TTL5m, TTL1h}}, {ProviderRoute: "five-only", TTLs: []TTL{TTL5m}}}},
	}
	f.db = openDB(t, f.path)
	var err error
	f.s, err = Create(ctx, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func hashString(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func (f *fixture) id() string    { f.seq++; return fmt.Sprintf("operation-%d", f.seq) }
func (f *fixture) command(op string, state State, ttl TTL) Command {
	return Command{Protocol: Protocol, Operation: op, Scope: f.scope, ContextEpoch: state.Active.ContextEpoch, ExpectedRevision: state.Revision, TTL: ttl, OperationID: f.id()}
}
func (f *fixture) initial() Command {
	return Command{Protocol: Protocol, Operation: "ensure", Scope: f.scope, ContextEpoch: "empty-epoch", ExpectedRevision: 0, TTL: TTL5m, OperationID: f.id(), Enrollment: "new_empty"}
}
func (f *fixture) apply(cmd Command) State {
	f.t.Helper()
	state, err := f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}
func (f *fixture) get() State {
	f.t.Helper()
	state, err := f.s.Get(ctx, f.auth, f.scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}
func ref(state State) Reference {
	return Reference{Scope: state.Scope, ContextEpoch: state.Active.ContextEpoch, Revision: state.Revision, TTL: state.Active.TTL}
}
func (f *fixture) request(state State, id string, kind Kind) Snapshot {
	f.t.Helper()
	snap, err := f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: id, Kind: kind})
	if err != nil {
		f.t.Fatal(err)
	}
	return snap
}
func (f *fixture) attempt(requestID, attemptID string) (Attempt, Snapshot) {
	f.t.Helper()
	a := Attempt{Scope: f.scope, RequestID: requestID, AttemptID: attemptID}
	snap, err := f.s.BeginAttempt(ctx, f.auth, a)
	if err != nil {
		f.t.Fatal(err)
	}
	return a, snap
}
func (f *fixture) commit(state State, epoch string) Command {
	c := f.command("commit", state, state.Pending.TTL)
	c.TransitionID = state.Pending.ID
	c.ContextEpoch, c.Checkpoint = epoch, epoch
	c.CheckpointReceipt = &CheckpointReceipt{Version: CheckpointReceiptVersion, TransitionID: state.Pending.ID, PreviousCheckpoint: state.Pending.FromCheckpoint, Checkpoint: epoch, Committed: true}
	c.CompactionStartedAt = f.clock.advance()
	c.CheckpointCommittedAt = f.clock.advance()
	return c
}
func (f *fixture) settle(a Attempt, outcome Outcome) {
	f.t.Helper()
	if err := f.s.FinishAttempt(ctx, f.auth, a, outcome); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.FinishRequest(ctx, f.auth, f.scope, a.RequestID, outcome); err != nil {
		f.t.Fatal(err)
	}
}
func requireCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != status || e.Code != "pacer_cache_"+code {
		t.Fatalf("got %v, want %d pacer_cache_%s", err, status, code)
	}
}

func TestEnrollmentAndEmptyChoice(t *testing.T) {
	f := newFixture(t)
	cmd := f.initial()
	_, err := f.s.Apply(ctx, f.auth, cmd, Evidence{})
	requireCode(t, err, 422, "enrollment_unsupported")
	// Failed operation receipts do not become success just because evidence changes.
	_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 422, "enrollment_unsupported")
	cmd.OperationID = f.id()
	state := f.apply(cmd)
	if state.Revision != 1 || state.Active.Used || state.Active.TTL != TTL5m {
		t.Fatalf("bad initial state: %+v", state)
	}
	change := f.command("ensure", state, TTL1h)
	state = f.apply(change)
	if state.Active.TTL != TTL1h || state.Revision != 2 {
		t.Fatal(state)
	}
	state2 := f.apply(f.command("ensure", state, TTL1h))
	if !reflect.DeepEqual(state, state2) {
		t.Fatal("same-TTL ensure mutated state")
	}
	stale := change
	stale.OperationID = f.id()
	stale.TTL = TTL5m
	_, err = f.s.Apply(ctx, f.auth, stale, Evidence{})
	requireCode(t, err, 409, "revision_conflict")
	f.request(state, "primary", Primary)
	if err = f.s.FinishRequest(ctx, f.auth, f.scope, "primary", Failed); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Apply(ctx, f.auth, f.command("ensure", state, TTL5m), Evidence{NewEmpty: true})
	requireCode(t, err, 409, "compaction_required")
	if !f.get().Active.Used {
		t.Fatal("failed primary reset used history")
	}
	changedEpoch := f.command("ensure", state, TTL1h)
	changedEpoch.ContextEpoch = "made-up-generation"
	_, err = f.s.Apply(ctx, f.auth, changedEpoch, Evidence{})
	requireCode(t, err, 409, "epoch_conflict")
}

func TestUnsupportedEnrollmentAndNoImplicitFreshState(t *testing.T) {
	f := newFixture(t)
	_, err := f.s.BeginRequest(ctx, f.auth, Request{Reference: Reference{Scope: f.scope, ContextEpoch: "unknown", Revision: 1, TTL: TTL5m}, RequestID: "unknown", Kind: Primary})
	requireCode(t, err, 409, "state_missing")
	for _, enrollment := range []string{"", "existing", "fork"} {
		cmd := f.initial()
		cmd.Enrollment = enrollment
		_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
		requireCode(t, err, 422, "enrollment_unsupported")
	}
	cmd := f.initial()
	cmd.ExpectedRevision = 7
	_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 409, "state_missing")
	cmd = f.initial()
	cmd.ProviderRoute = "unsupported"
	_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 422, "unsupported")
	cmd = f.initial()
	cmd.TTL = TTL("2h")
	_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 422, "unsupported")
	cmd = f.initial()
	cmd.ProviderRoute = "five-only"
	cmd.TTL = TTL1h
	_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 422, "unsupported")
	f.apply(f.initial())
	cmd = f.initial()
	cmd.ProviderRoute = "five-only"
	_, err = f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 422, "enrollment_unsupported")
}

func TestTransitionDrainsPrimaryPausesWarmingAndAllowsSummary(t *testing.T) {
	f := newFixture(t)
	state := f.apply(f.initial())
	f.request(state, "primary", Primary)
	primary, original := f.attempt("primary", "primary-a1")
	f.request(state, "warm", Warming)
	warm, _ := f.attempt("warm", "warm-a1")
	prepare := f.command("prepare", state, TTL1h)
	pending := f.apply(prepare)
	if !pending.WarmingPaused || pending.Active.TTL != TTL5m || pending.Active.ContextEpoch != "empty-epoch" || pending.ActivePrimary != 1 || pending.ActiveWarming != 1 || pending.ActiveAttempts != 2 {
		t.Fatalf("bad pending state %+v", pending)
	}
	before := f.get()
	after := f.get()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("GET mutated policy")
	}
	for _, kind := range []Kind{Primary, Warming} {
		_, err := f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(pending), RequestID: string(kind) + "-blocked", Kind: kind})
		requireCode(t, err, 409, "transition_pending")
	}
	_, err := f.s.BeginAttempt(ctx, f.auth, Attempt{Scope: f.scope, RequestID: "primary", AttemptID: "primary-a2"})
	requireCode(t, err, 409, "transition_pending")
	if err = f.s.ValidateAttempt(ctx, f.auth, original); err != nil {
		t.Fatal(err)
	}
	if snap, err := f.s.BeginAttempt(ctx, f.auth, primary); err != nil || !reflect.DeepEqual(snap, original) {
		t.Fatalf("lost attempt response changed: %+v %v", snap, err)
	}
	summary := f.request(pending, "summary", Auxiliary)
	if summary.Managed || summary.TTL != "" {
		t.Fatalf("summary inherited primary policy: %+v", summary)
	}
	aux, auxSnap := f.attempt("summary", "summary-a1")
	if auxSnap.Managed || auxSnap.TTL != "" || f.get().ActiveAttempts != 2 {
		t.Fatal("summary counts as primary attempt")
	}
	commit := f.commit(pending, "checkpoint-1")
	_, err = f.s.Apply(ctx, f.auth, commit, Evidence{})
	requireCode(t, err, 409, "inflight")
	err = f.s.FinishRequest(ctx, f.auth, f.scope, primary.RequestID, Succeeded)
	requireCode(t, err, 409, "inflight")
	f.settle(primary, Succeeded)
	_, err = f.s.Apply(ctx, f.auth, f.commit(pending, "checkpoint-1"), Evidence{})
	requireCode(t, err, 409, "inflight")
	f.settle(warm, Failed)
	// The failed operation stays failed even when its precondition later clears.
	_, err = f.s.Apply(ctx, f.auth, commit, Evidence{})
	requireCode(t, err, 409, "inflight")
	commit.OperationID = f.id()
	state = f.apply(commit)
	if state.Active.TTL != TTL1h || state.Active.ContextEpoch != "checkpoint-1" || state.Pending != nil || state.WarmingPaused || state.ActiveAuxiliary != 1 || !state.Active.Used {
		t.Fatalf("bad committed policy: %+v", state)
	}
	if got := f.apply(commit); !reflect.DeepEqual(got, state) {
		t.Fatal("lost commit response was not idempotent")
	}
	if got := f.apply(prepare); !reflect.DeepEqual(got, pending) {
		t.Fatal("prepare replay did not retain original receipt")
	}
	if got, err := f.s.GetAttempt(ctx, f.auth, primary); err != nil || !reflect.DeepEqual(got, original) {
		t.Fatalf("late usage changed epochs: %+v %v", got, err)
	}
	if err = f.s.ValidateAttempt(ctx, f.auth, auxSnap); err != nil {
		t.Fatal(err)
	}
	f.settle(aux, Succeeded)
	_, err = f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(pending), RequestID: "stale", Kind: Primary})
	requireCode(t, err, 409, "revision_conflict")
	_, err = f.s.Apply(ctx, f.auth, f.command("ensure", state, TTL5m), Evidence{})
	requireCode(t, err, 409, "compaction_required")
}

func TestFreshCheckpointRulesCancelAndOrdinaryCompaction(t *testing.T) {
	f := newFixture(t)
	state := f.apply(f.initial())
	pending := f.apply(f.command("prepare", state, TTL1h))
	good := f.commit(pending, "checkpoint-1")
	cases := []struct {
		name   string
		change func(*Command)
	}{
		{"unchanged", func(c *Command) { c.Checkpoint = state.Active.Checkpoint; c.ContextEpoch = c.Checkpoint }},
		{"different-epoch", func(c *Command) { c.ContextEpoch = "not-the-checkpoint" }},
		{"before-prepare-receipt", func(c *Command) { c.CheckpointReceipt.TransitionID = "previous-transition" }},
		{"wrong-previous-checkpoint", func(c *Command) { c.CheckpointReceipt.PreviousCheckpoint = "another-checkpoint" }},
		{"not-committed", func(c *Command) { c.CheckpointReceipt.Committed = false }},
		{"wrong-checkpoint-receipt", func(c *Command) { c.CheckpointReceipt.Checkpoint = "not-committed-checkpoint" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := good
			receipt := *good.CheckpointReceipt
			cmd.CheckpointReceipt = &receipt
			cmd.OperationID = f.id()
			tc.change(&cmd)
			_, err := f.s.Apply(ctx, f.auth, cmd, Evidence{})
			requireCode(t, err, 409, "checkpoint_required")
			if !reflect.DeepEqual(f.get().Active, state.Active) {
				t.Fatal("failed compaction changed policy")
			}
		})
	}
	cancel := f.command("cancel", pending, TTL1h)
	cancel.TransitionID = pending.Pending.ID
	state = f.apply(cancel)
	if state.Pending != nil || state.Active.TTL != TTL5m {
		t.Fatal(state)
	}
	_, err := f.s.Apply(ctx, f.auth, good, Evidence{})
	requireCode(t, err, 409, "revision_conflict")
	// Ordinary compaction keeps TTL but creates a real new context epoch.
	pending = f.apply(f.command("prepare", state, TTL5m))
	state = f.apply(f.commit(pending, "checkpoint-ordinary"))
	if state.Active.TTL != TTL5m || state.Active.ContextEpoch != "checkpoint-ordinary" {
		t.Fatal(state)
	}
	pending = f.apply(f.command("prepare", state, TTL1h))
	_, err = f.s.Apply(ctx, f.auth, f.commit(pending, "empty-epoch"), Evidence{})
	requireCode(t, err, 409, "checkpoint_required")
}

func TestLostResponseRestartAndExplicitRecovery(t *testing.T) {
	f := newFixture(t)
	ensure := f.initial()
	state := f.apply(ensure)
	f.request(state, "primary", Primary)
	attempt, old := f.attempt("primary", "attempt-1")
	prepare := f.command("prepare", state, TTL1h)
	pending := f.apply(prepare)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db = openDB(t, f.path)
	var err error
	f.s, err = Open(ctx, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.get(); !reflect.DeepEqual(got, pending) {
		t.Fatalf("restart forgot state: %+v", got)
	}
	if got := f.apply(ensure); got.Revision != 1 {
		t.Fatal("initial lost response not retained")
	}
	if got := f.apply(prepare); !reflect.DeepEqual(got, pending) {
		t.Fatal("prepare lost response not retained")
	}
	items, err := f.s.Outstanding(ctx, f.auth, f.scope)
	if err != nil || len(items) != 2 {
		t.Fatalf("outstanding: %+v %v", items, err)
	}
	_, err = f.s.Apply(ctx, f.auth, f.commit(pending, "checkpoint-restart"), Evidence{})
	requireCode(t, err, 409, "inflight")
	f.settle(attempt, Failed) // host has now confirmed old dispatch stopped
	commit := f.commit(pending, "checkpoint-restart")
	state = f.apply(commit)
	if state.Active.TTL != TTL1h {
		t.Fatal(state)
	}
	got, err := f.s.GetAttempt(ctx, f.auth, attempt)
	if err != nil || !reflect.DeepEqual(got, old) {
		t.Fatalf("old immutable attempt missing: %+v %v", got, err)
	}
	items, err = f.s.Outstanding(ctx, f.auth, f.scope)
	if err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	if err = f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db = openDB(t, f.path)
	f.s, err = Open(ctx, f.db, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if replay := f.apply(commit); !reflect.DeepEqual(replay, state) {
		t.Fatal("lost committed response changed after restart")
	}
	_, err = Create(ctx, f.db, f.opts)
	requireCode(t, err, 503, "unavailable")
	missingDB := openDB(t, filepath.Join(t.TempDir(), "lost.sqlite"))
	_, err = Open(ctx, missingDB, f.opts)
	requireCode(t, err, 503, "unavailable")
	var tables int
	if err = missingDB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'cache_policy_%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal("Open created fresh policy state", tables, err)
	}
}

func TestForeignScopeCapabilityAndImmutableSnapshots(t *testing.T) {
	f := newFixture(t)
	cmd := f.initial()
	state := f.apply(cmd)
	f.request(state, "primary", Primary)
	a, original := f.attempt("primary", "attempt-1")
	for _, auth := range []Credentials{
		{CallerDigest: hashString("foreign"), Capability: f.auth.Capability},
		{CallerDigest: f.auth.CallerDigest, Capability: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))},
		{CallerDigest: "raw-client-key", Capability: f.auth.Capability},
		{CallerDigest: f.auth.CallerDigest, Capability: ""},
	} {
		if _, err := f.s.Get(ctx, auth, f.scope); err == nil {
			t.Fatal("foreign read accepted")
		}
		if _, err := f.s.Apply(ctx, auth, cmd, Evidence{}); err == nil {
			t.Fatal("foreign replay accepted")
		}
		if _, err := f.s.GetAttempt(ctx, auth, a); err == nil {
			t.Fatal("foreign historical usage accepted")
		}
		if err := f.s.FinishAttempt(ctx, auth, a, Failed); err == nil {
			t.Fatal("foreign settlement accepted")
		}
	}
	for _, change := range []func(*Scope){
		func(s *Scope) { s.InstallationID = "foreign-install" },
		func(s *Scope) { s.SessionID = "foreign-session" },
		func(s *Scope) { s.ProviderRoute = "five-only" },
	} {
		scope := f.scope
		change(&scope)
		if _, err := f.s.Get(ctx, f.auth, scope); err == nil {
			t.Fatal("foreign scope read accepted")
		}
		other := a
		other.Scope = scope
		if _, err := f.s.GetAttempt(ctx, f.auth, other); err == nil {
			t.Fatal("foreign attempt accepted")
		}
	}
	for _, change := range []func(*Snapshot){
		func(s *Snapshot) { s.TTL = TTL1h },
		func(s *Snapshot) { s.Revision++ },
		func(s *Snapshot) { s.ContextEpoch = "fake" },
		func(s *Snapshot) { s.StartedAt = s.StartedAt.Add(time.Second) },
		func(s *Snapshot) { s.Kind = Warming },
	} {
		fake := original
		change(&fake)
		err := f.s.ValidateAttempt(ctx, f.auth, fake)
		requireCode(t, err, 409, "snapshot_conflict")
	}
	if err := f.s.ValidateAttempt(ctx, f.auth, original); err != nil {
		t.Fatal(err)
	}
	f.settle(a, Succeeded)
	err := f.s.ValidateAttempt(ctx, f.auth, original)
	requireCode(t, err, 409, "attempt_settled")
	// The capability is never persisted as plaintext in correctness rows.
	rows, err := f.db.Query(`SELECT capability_hash || CAST(state AS TEXT) FROM cache_policy_scopes UNION ALL SELECT capability_hash || COALESCE(CAST(result AS TEXT),'') FROM cache_policy_operations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, f.auth.Capability) {
			t.Fatal("private capability persisted")
		}
	}
}

func TestRequestAttemptDeduplicationAndOutcomes(t *testing.T) {
	f := newFixture(t)
	state := f.apply(f.initial())
	original := f.request(state, "primary", Primary)
	if again := f.request(state, "primary", Primary); !reflect.DeepEqual(again, original) {
		t.Fatal("request snapshot changed")
	}
	_, err := f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "primary", Kind: Warming})
	requireCode(t, err, 409, "request_reused")
	a, _ := f.attempt("primary", "attempt-1")
	f.attempt("primary", "attempt-1")
	if got := f.get(); got.ActivePrimary != 1 || got.ActiveAttempts != 1 {
		t.Fatal("duplicate inflated accounting", got)
	}
	if err = f.s.FinishAttempt(ctx, f.auth, a, Failed); err != nil {
		t.Fatal(err)
	}
	if err = f.s.FinishAttempt(ctx, f.auth, a, Failed); err != nil {
		t.Fatal(err)
	}
	err = f.s.FinishAttempt(ctx, f.auth, a, Succeeded)
	requireCode(t, err, 409, "outcome_conflict")
	_, err = f.s.BeginAttempt(ctx, f.auth, a)
	requireCode(t, err, 409, "attempt_settled")
	retry, _ := f.attempt("primary", "attempt-2")
	f.request(state, "other", Primary)
	_, err = f.s.BeginAttempt(ctx, f.auth, Attempt{Scope: f.scope, RequestID: "other", AttemptID: "attempt-2"})
	requireCode(t, err, 409, "attempt_reused")
	f.settle(retry, Succeeded)
	if err = f.s.FinishRequest(ctx, f.auth, f.scope, "primary", Succeeded); err != nil {
		t.Fatal(err)
	}
	err = f.s.FinishRequest(ctx, f.auth, f.scope, "primary", Failed)
	requireCode(t, err, 409, "outcome_conflict")
	_, err = f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "primary", Kind: Primary})
	requireCode(t, err, 409, "request_settled")
	_, err = f.s.BeginAttempt(ctx, f.auth, Attempt{Scope: f.scope, RequestID: "primary", AttemptID: "after-settled"})
	requireCode(t, err, 409, "request_settled")
	if err = f.s.FinishRequest(ctx, f.auth, f.scope, "other", Canceled); err != nil {
		t.Fatal(err)
	}
	if got := f.get(); got.ActivePrimary != 0 || got.ActiveAttempts != 0 {
		t.Fatal(got)
	}
}

func TestConcurrentCASAndLostResponseAcrossDatabaseHandles(t *testing.T) {
	f := newFixture(t)
	state := f.apply(f.initial())
	otherDB := openDB(t, f.path)
	other, err := Open(ctx, otherDB, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*Store{f.s, other}
	const n = 16
	var wg sync.WaitGroup
	var wins atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := Command{Protocol: Protocol, Operation: "ensure", Scope: f.scope, ContextEpoch: state.Active.ContextEpoch, ExpectedRevision: state.Revision, TTL: TTL1h, OperationID: fmt.Sprintf("race-%d", i)}
			_, err := stores[i%2].Apply(ctx, f.auth, cmd, Evidence{})
			if err == nil {
				wins.Add(1)
			} else {
				requireCode(t, err, 409, "revision_conflict")
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("CAS winners = %d", wins.Load())
	}
	state = f.get()
	cmd := f.command("prepare", state, TTL5m)
	results := make(chan State, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := stores[i%2].Apply(ctx, f.auth, cmd, Evidence{})
			if err != nil {
				t.Error(err)
				return
			}
			results <- got
		}(i)
	}
	wg.Wait()
	close(results)
	pending := f.get()
	for got := range results {
		if !reflect.DeepEqual(got, pending) {
			t.Error("same operation got different transition receipts")
		}
	}
	changed := cmd
	changed.TTL = TTL1h
	_, err = f.s.Apply(ctx, f.auth, changed, Evidence{})
	requireCode(t, err, 409, "operation_reused")
	otherScope := cmd
	otherScope.SessionID = "other"
	_, err = f.s.Apply(ctx, f.auth, otherScope, Evidence{})
	requireCode(t, err, 409, "operation_reused")
}

func TestConcurrentLifecycleAndPrepareAdmissionRace(t *testing.T) {
	f := newFixture(t)
	state := f.apply(f.initial())
	other, err := Open(ctx, openDB(t, f.path), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := other.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: fmt.Sprintf("parallel-%d", i), Kind: Primary})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := f.get(); got.ActivePrimary != n {
		t.Fatalf("lost lifecycle increments %+v", got)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := other.FinishRequest(ctx, f.auth, f.scope, fmt.Sprintf("parallel-%d", i), Canceled); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := f.get(); got.ActivePrimary != 0 {
		t.Fatalf("lost lifecycle decrements %+v", got)
	}
	start := make(chan struct{})
	admitted := make(chan bool, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, err := other.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "racing", Kind: Warming})
		if err != nil {
			requireCode(t, err, 409, "revision_conflict")
		}
		admitted <- err == nil
	}()
	close(start)
	pending := f.apply(f.command("prepare", state, TTL1h))
	wg.Wait()
	if <-admitted {
		_, err := f.s.Apply(ctx, f.auth, f.commit(pending, "race-checkpoint"), Evidence{})
		requireCode(t, err, 409, "inflight")
		if err = other.FinishRequest(ctx, f.auth, f.scope, "racing", Canceled); err != nil {
			t.Fatal(err)
		}
	}
	f.apply(f.commit(pending, "race-checkpoint"))
}

func TestStorageFailureRollsBackAdmissionAndTransition(t *testing.T) {
	f := newFixture(t)
	state := f.apply(f.initial())
	_, err := f.db.Exec(`CREATE TRIGGER deny_policy_update BEFORE UPDATE ON cache_policy_scopes BEGIN SELECT RAISE(ABORT, 'injected local failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "failed-insert", Kind: Primary})
	requireCode(t, err, 503, "unavailable")
	var count int
	if err = f.db.QueryRow(`SELECT count(*) FROM cache_policy_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial admission persisted", count, err)
	}
	if got := f.get(); !reflect.DeepEqual(got, state) {
		t.Fatal("failure changed active policy")
	}
	if _, err = f.db.Exec(`DROP TRIGGER deny_policy_update`); err != nil {
		t.Fatal(err)
	}
	pending := f.apply(f.command("prepare", state, TTL1h))
	commit := f.commit(pending, "checkpoint-1")
	// Fail after state/epoch writes, at operation receipt insertion.
	if _, err = f.db.Exec(`CREATE TRIGGER deny_operation BEFORE INSERT ON cache_policy_operations BEGIN SELECT RAISE(ABORT, 'injected local failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Apply(ctx, f.auth, commit, Evidence{})
	requireCode(t, err, 503, "unavailable")
	if got := f.get(); !reflect.DeepEqual(got, pending) {
		t.Fatal("partial commit persisted")
	}
	if err = f.db.QueryRow(`SELECT count(*) FROM cache_policy_epochs WHERE context_epoch='checkpoint-1'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial epoch persisted", count, err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER deny_operation`); err != nil {
		t.Fatal(err)
	}
	f.apply(commit) // failed transaction did not consume the operation ID
	if err = f.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Get(ctx, f.auth, f.scope)
	requireCode(t, err, 503, "unavailable")
	_, err = f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "storage-gone", Kind: Primary})
	requireCode(t, err, 503, "unavailable")
}

func TestLostStateCannotBeReconstitutedByOperationReceipt(t *testing.T) {
	f := newFixture(t)
	cmd := f.initial()
	state := f.apply(cmd)
	if _, err := f.db.Exec(`DELETE FROM cache_policy_scopes`); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Apply(ctx, f.auth, cmd, Evidence{NewEmpty: true})
	requireCode(t, err, 409, "state_missing")
	_, err = f.s.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "missing", Kind: Primary})
	requireCode(t, err, 409, "state_missing")
	_, err = f.s.Apply(ctx, f.auth, f.command("ensure", state, TTL5m), Evidence{NewEmpty: true})
	requireCode(t, err, 409, "state_missing")
	// Even an incorrect new-empty attestation cannot overwrite retained enrollment evidence.
	_, err = f.s.Apply(ctx, f.auth, f.initial(), Evidence{NewEmpty: true})
	requireCode(t, err, 409, "state_missing")
}

func TestSupportRemovalDoesNotDowngradeAndStateCorruptionFails(t *testing.T) {
	f := newFixture(t)
	cmd := f.initial()
	cmd.TTL = TTL1h
	state := f.apply(cmd)
	limited, err := Open(ctx, f.db, Options{Clock: f.clock.now, Routes: []Route{{ProviderRoute: "claude-main", TTLs: []TTL{TTL5m}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = limited.BeginRequest(ctx, f.auth, Request{Reference: ref(state), RequestID: "unsupported", Kind: Primary})
	requireCode(t, err, 422, "unsupported")
	if got := f.get(); got.Active.TTL != TTL1h {
		t.Fatal("silently downgraded")
	}
	if _, err = f.db.Exec(`UPDATE cache_policy_scopes SET state='not JSON'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Get(ctx, f.auth, f.scope)
	requireCode(t, err, 503, "unavailable")
	if _, err = f.db.Exec(`UPDATE cache_policy_meta SET version=99`); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, f.db, f.opts)
	requireCode(t, err, 503, "unavailable")
}

func TestStrictWireContract(t *testing.T) {
	f := newFixture(t)
	cmd := f.initial()
	raw, err := json.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCommand(strings.NewReader(string(raw)))
	if err != nil || !reflect.DeepEqual(cmd, decoded) {
		t.Fatal(decoded, err)
	}
	for _, input := range []string{
		strings.TrimSuffix(string(raw), "}") + `,"caller_digest":"untrusted"}`,
		strings.TrimSuffix(string(raw), "}") + `,"capability":"untrusted"}`,
		strings.TrimSuffix(string(raw), "}") + `,"generation":5}`,
		strings.TrimSuffix(string(raw), "}") + `,"skip_compaction":true}`,
		string(raw) + ` {}`,
		strings.Repeat(" ", MaxCommandBytes+1),
		`null`,
	} {
		if _, err := DecodeCommand(strings.NewReader(input)); err == nil {
			t.Fatalf("invalid wire input accepted: %.100s", input)
		}
	}
}
