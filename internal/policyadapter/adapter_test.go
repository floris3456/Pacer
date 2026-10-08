package policyadapter

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

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var testContext = context.Background()

type fixture struct {
	t          *testing.T
	a          *Adapter
	db         *sql.DB
	path       string
	opts       Options
	scope      pluginapi.CachePolicyScope
	capability string
	clock      atomic.Int64
	seq        int
}

func testDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	return db
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	caller := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00synthetic-principal"))
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "adapter.sqlite"),
		scope:      pluginapi.CachePolicyScope{CallerScope: hex.EncodeToString(caller[:]), InstallationID: "installation", SessionID: "session", ProviderRoute: "claude-main"},
		capability: base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))}
	f.clock.Store(time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC).UnixNano())
	f.opts = Options{Clock: f.now, Routes: []Route{
		{ProviderRoute: "claude-main", Models: []string{"claude-opus-4-6", "claude-sonnet-4-6"}, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}},
		{ProviderRoute: "claude-small", Models: []string{"claude-haiku-4-5"}, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m}},
	}}
	f.db = testDB(t, f.path)
	policy, err := PolicyOptions(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit first installation, never an adapter request-path fallback.
	if _, err = cachepolicy.Create(testContext, f.db, policy); err != nil {
		t.Fatal(err)
	}
	f.open()
	return f
}

func (f *fixture) now() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
func (f *fixture) next() string   { f.seq++; return fmt.Sprintf("operation-%d", f.seq) }
func (f *fixture) open() {
	f.t.Helper()
	var err error
	f.a, err = Open(testContext, f.db, f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) restart() {
	f.t.Helper()
	if err := f.db.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.db = testDB(f.t, f.path)
	f.open()
}

func call[T any](ctx context.Context, a *Adapter, method string, input any) (T, error) {
	var out T
	raw, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	result, err := a.Dispatch(ctx, method, raw)
	if err != nil {
		return out, err
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func checkError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var e *pluginabi.Error
	if !errors.As(err, &e) || e.HTTPStatus != status || e.Code != "pacer_cache_"+code {
		t.Fatalf("got %v; want %d pacer_cache_%s", err, status, code)
	}
}

func (f *fixture) operation(kind string, state cachepolicy.State, ttl string) pluginapi.CachePolicyOperation {
	return pluginapi.CachePolicyOperation{Protocol: 1, Operation: kind, InstallationID: f.scope.InstallationID, SessionID: f.scope.SessionID, ProviderRoute: f.scope.ProviderRoute, ContextEpoch: state.Active.ContextEpoch, ExpectedRevision: state.Revision, TTL: ttl, OperationID: f.next()}
}

func (f *fixture) control(method string, op pluginapi.CachePolicyOperation, status int, code string) cachepolicy.State {
	f.t.Helper()
	resp, err := call[pluginapi.CachePolicyResponse](testContext, f.a, pluginabi.MethodCachePolicyHandle, pluginapi.CachePolicyRequest{Method: method, Scope: f.scope, Capability: f.capability, Operation: op})
	if err != nil {
		f.t.Fatal(err)
	}
	return controlState(f.t, resp, status, code)
}

func controlState(t *testing.T, resp pluginapi.CachePolicyResponse, status int, code string) cachepolicy.State {
	t.Helper()
	if resp.StatusCode != status || !json.Valid(resp.Body) {
		t.Fatalf("response: %d %s, want %d", resp.StatusCode, resp.Body, status)
	}
	if status != 200 {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "pacer_cache_"+code {
			t.Fatalf("error %s, want %s", resp.Body, code)
		}
		return cachepolicy.State{}
	}
	var state cachepolicy.State
	if err := json.Unmarshal(resp.Body, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *fixture) read() cachepolicy.State {
	return f.control("GET", pluginapi.CachePolicyOperation{Protocol: 1, Operation: "read", InstallationID: f.scope.InstallationID, SessionID: f.scope.SessionID, ProviderRoute: f.scope.ProviderRoute}, 200, "")
}

func (f *fixture) prepareExisting() pluginapi.CachePolicyOperation {
	c := f.operation("prepare", cachepolicy.State{Active: cachepolicy.Policy{ContextEpoch: "old-context"}}, "5m")
	c.Enrollment = "existing_history"
	return c
}

func (f *fixture) commit(pending cachepolicy.State, epoch string) pluginapi.CachePolicyOperation {
	c := f.operation("commit", pending, string(pending.Pending.TTL))
	c.ContextEpoch, c.Checkpoint, c.TransitionID = epoch, epoch, pending.Pending.ID
	c.CheckpointReceipt = &pluginapi.CachePolicyCheckpointReceipt{Version: 1, TransitionID: pending.Pending.ID, PreviousCheckpoint: pending.Pending.FromCheckpoint, Checkpoint: epoch, Committed: true}
	return c
}

func (f *fixture) enroll() cachepolicy.State {
	f.t.Helper()
	pending := f.control("POST", f.prepareExisting(), 200, "")
	return f.control("POST", f.commit(pending, "initial-checkpoint"), 200, "")
}

func (f *fixture) reference(state cachepolicy.State, requestID string) pluginapi.CachePolicyResolveRequest {
	return pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: f.scope, Capability: f.capability, ContextEpoch: state.Active.ContextEpoch, Revision: state.Revision, Kind: "primary", RequestID: requestID, Model: "claude-opus-4-6", SourceFormat: "claude"}
}

func (f *fixture) resolve(req pluginapi.CachePolicyResolveRequest) pluginapi.CachePolicy {
	f.t.Helper()
	p, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, req)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) begin(policy pluginapi.CachePolicy, requestID, attemptID string) pluginapi.CachePolicyAttemptRequest {
	f.t.Helper()
	r := pluginapi.CachePolicyAttemptRequest{Operation: "begin", RequestID: requestID, AttemptID: attemptID, Capability: f.capability, Policy: policy}
	p, err := call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, r)
	if err != nil {
		f.t.Fatal(err)
	}
	if p.Policy != policy || !p.StartedAt.Equal(f.now()) {
		f.t.Fatal("attempt did not pin the exact resolved policy/start", p)
	}
	r.StartedAt = p.StartedAt
	return r
}

func (f *fixture) validate(r pluginapi.CachePolicyAttemptRequest) {
	f.t.Helper()
	r.Operation = "validate"
	result, err := call[map[string]any](testContext, f.a, pluginabi.MethodCachePolicyAttempt, r)
	if err != nil || len(result) != 0 {
		f.t.Fatalf("validate: %v %v", result, err)
	}
}

func (f *fixture) finish(r pluginapi.CachePolicyAttemptRequest, outcome string, dispatched bool) error {
	r.Operation, r.Outcome, r.Dispatched = "finish", outcome, dispatched
	_, err := call[map[string]any](testContext, f.a, pluginabi.MethodCachePolicyAttempt, r)
	return err
}

func (f *fixture) complete(id string, outcome pluginapi.RequestCompletionOutcome) error {
	_, err := call[map[string]any](testContext, f.a, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: id, Outcome: outcome})
	return err
}

func TestDefaultAttestationGateExistingEnrollmentAndReadonlyGET(t *testing.T) {
	f := newFixture(t)
	empty := f.operation("ensure", cachepolicy.State{Active: cachepolicy.Policy{ContextEpoch: "empty"}}, "1h")
	empty.Enrollment = "new_empty"
	f.control("POST", empty, 422, "enrollment_unsupported")
	prepare := f.prepareExisting()
	pending := f.control("POST", prepare, 200, "")
	if !pending.EnrollmentRequired || pending.Active.TTL != "" || !pending.WarmingPaused {
		t.Fatal(pending)
	}
	_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, f.reference(pending, "not-yet"))
	checkError(t, err, 409, "enrollment_required")
	f.control("GET", prepare, 400, "invalid")
	if got := f.read(); !reflect.DeepEqual(got, pending) {
		t.Fatal("GET mutated state")
	}
	commit := f.commit(pending, "new-checkpoint")
	ahead := f.now().Add(time.Second)
	commit.CompactionStartedAt, commit.CheckpointCommittedAt = &ahead, &ahead
	active := f.control("POST", commit, 200, "")
	if active.LastCommit == nil || !active.LastCommit.CheckpointCommittedAt.Equal(ahead) {
		t.Fatal("CPA pointer timestamps/receipt were lost")
	}
	f.restart()
	if got := f.control("POST", commit, 200, ""); !reflect.DeepEqual(got, active) {
		t.Fatal("lost response replay changed after restart")
	}
	var storedCaller string
	if err = f.db.QueryRow(`SELECT caller_digest FROM cache_policy_scopes`).Scan(&storedCaller); err != nil || storedCaller != f.scope.CallerScope {
		t.Fatal("caller digest was hashed twice", storedCaller, err)
	}
	raw, _ := json.Marshal(active)
	if strings.Contains(string(raw), f.capability) {
		t.Fatal("capability leaked to policy response")
	}
}

func TestInjectableAuthorityIsSeparateAndFailClosed(t *testing.T) {
	f := newFixture(t)
	var approved atomic.Bool
	f.opts.NewEmptyAuthority = func(_ context.Context, claim EmptyClaim) (bool, error) {
		raw, _ := json.Marshal(claim)
		if strings.Contains(string(raw), f.capability) {
			t.Fatal("hook leaked private capability")
		}
		return approved.Load(), nil
	}
	f.open()
	cmd := f.operation("ensure", cachepolicy.State{Active: cachepolicy.Policy{ContextEpoch: "empty"}}, "5m")
	cmd.Enrollment = "new_empty"
	f.control("POST", cmd, 422, "enrollment_unsupported")
	approved.Store(true)
	cmd.OperationID = f.next()
	state := f.control("POST", cmd, 200, "")
	if state.Active.Used || state.EnrollmentRequired {
		t.Fatal(state)
	}
	approved.Store(false)
	if replay := f.control("POST", cmd, 200, ""); !reflect.DeepEqual(replay, state) {
		t.Fatal("recorded enrollment was not replayable")
	}
	f.opts.NewEmptyAuthority = func(context.Context, EmptyClaim) (bool, error) {
		return false, errors.New("secret-hook-error-" + f.capability)
	}
	f.open()
	cmd.OperationID = f.next()
	resp, err := call[pluginapi.CachePolicyResponse](testContext, f.a, pluginabi.MethodCachePolicyHandle, pluginapi.CachePolicyRequest{Method: "POST", Scope: f.scope, Capability: f.capability, Operation: cmd})
	if err != nil {
		t.Fatal(err)
	}
	controlState(t, resp, 503, "attestation_unavailable")
	if strings.Contains(string(resp.Body), "secret-hook-error") || strings.Contains(string(resp.Body), f.capability) {
		t.Fatal("authority error leaked secrets")
	}
}

func TestScopeRouteModelAndTransportRefusals(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	base := f.reference(state, "logical")
	for _, mutate := range []func(*pluginapi.CachePolicyResolveRequest){
		func(r *pluginapi.CachePolicyResolveRequest) { r.Scope.ProviderRoute = "unconfigured" },
		func(r *pluginapi.CachePolicyResolveRequest) { r.Model = "gpt-6" },
		func(r *pluginapi.CachePolicyResolveRequest) { r.Model = "claude-haiku-4-5" },
		func(r *pluginapi.CachePolicyResolveRequest) { r.Model = "my-claude-alias" },
		func(r *pluginapi.CachePolicyResolveRequest) { r.SourceFormat = "openai" },
		func(r *pluginapi.CachePolicyResolveRequest) { r.SourceFormat = "websocket" },
		func(r *pluginapi.CachePolicyResolveRequest) { r.Kind = "auxiliary" },
	} {
		r := base
		mutate(&r)
		_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, r)
		checkError(t, err, 422, "unsupported")
	}
	wrongCap := base
	wrongCap.Capability = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))
	_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, wrongCap)
	checkError(t, err, 403, "scope_denied")
	foreign := base
	foreign.Scope.CallerScope = strings.Repeat("a", 64)
	_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, foreign)
	checkError(t, err, 409, "state_missing")
	for _, mutate := range []func(*pluginapi.CachePolicyScope){
		func(s *pluginapi.CachePolicyScope) { s.InstallationID = "foreign" },
		func(s *pluginapi.CachePolicyScope) { s.SessionID = "foreign" },
	} {
		r := base
		mutate(&r.Scope)
		_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, r)
		checkError(t, err, 409, "state_missing")
	}
	read := pluginapi.CachePolicyOperation{Protocol: 1, Operation: "read", InstallationID: f.scope.InstallationID, SessionID: "foreign", ProviderRoute: f.scope.ProviderRoute}
	resp, err := call[pluginapi.CachePolicyResponse](testContext, f.a, pluginabi.MethodCachePolicyHandle, pluginapi.CachePolicyRequest{Method: "GET", Scope: f.scope, Capability: f.capability, Operation: read})
	if err != nil {
		t.Fatal(err)
	}
	controlState(t, resp, 403, "scope_denied")
	if got := f.read(); got.ActivePrimary != 0 {
		t.Fatal("refused resolves created guards")
	}
	// The adapter copies configured allowlists; later caller mutation grants nothing.
	f.opts.Routes[0].Models[0] = "claude-unconfigured"
	p := f.resolve(base)
	if p.Scope.CallerScope != f.scope.CallerScope {
		t.Fatal("caller identity changed")
	}
	r := base
	r.Model = "claude-unconfigured"
	r.RequestID = "mutated-config"
	_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, r)
	checkError(t, err, 422, "unsupported")
	foreign = base
	foreign.Scope.CallerScope = strings.Repeat("a", 64)
	_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, foreign)
	checkError(t, err, 403, "scope_denied") // process-global lifecycle ID cannot be rebound
}

func TestFrozenAttemptsGuardCommitAndCompletionForBothFormats(t *testing.T) {
	for _, format := range []string{"claude", "openai-response"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				f := newFixture(t)
				state := f.enroll()
				req := f.reference(state, "logical")
				req.SourceFormat, req.Stream = format, stream
				policy := f.resolve(req)
				if again := f.resolve(req); again != policy {
					t.Fatal("resolve response loss changed frozen policy")
				}
				attempt := f.begin(policy, "logical", "physical-1")
				f.validate(attempt)
				pending := f.control("POST", f.operation("prepare", state, "1h"), 200, "")
				retry := attempt
				retry.Operation, retry.AttemptID, retry.StartedAt = "begin", "physical-2", time.Time{}
				_, err := call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, retry)
				checkError(t, err, 409, "transition_pending")
				f.validate(attempt) // already-started old snapshot remains valid during prepare
				f.control("POST", f.commit(pending, "compacted"), 409, "inflight")
				if err = f.finish(attempt, "succeeded", true); err != nil {
					t.Fatal(err)
				}
				if got := f.read(); got.ActiveAttempts != 0 || got.ActivePrimary != 1 {
					t.Fatal("physical finish released logical guard", got)
				}
				f.control("POST", f.commit(pending, "compacted"), 409, "inflight")
				completion := pluginapi.RequestCompletion{RequestID: "logical", SourceFormat: format, Model: req.Model, Stream: stream, Outcome: pluginapi.RequestCompletionSucceeded, Metadata: map[string]any{"caller_scope": "untrusted", "capability": "do-not-use"}}
				if _, err = call[map[string]any](testContext, f.a, pluginabi.MethodRequestComplete, completion); err != nil {
					t.Fatal(err)
				}
				if len(f.a.requests) != 0 {
					t.Fatal("private lifecycle credentials retained after settlement")
				}
				active := f.control("POST", f.commit(pending, "compacted"), 200, "")
				if active.Active.TTL != cachepolicy.TTL1h {
					t.Fatal(active)
				}
				old, err := f.a.store.GetAttempt(testContext, cachepolicy.Credentials{CallerDigest: f.scope.CallerScope, Capability: f.capability}, cachepolicy.Attempt{Scope: scope(f.scope), RequestID: "logical", AttemptID: "physical-1"})
				if err != nil || old.TTL != cachepolicy.TTL5m || old.ContextEpoch != state.Active.ContextEpoch {
					t.Fatal("late usage joined current policy", old, err)
				}
				raw, _ := json.Marshal(struct {
					Policy   pluginapi.CachePolicy
					Snapshot cachepolicy.Snapshot
				}{policy, old})
				if strings.Contains(string(raw), f.capability) {
					t.Fatal("private capability leaked into snapshot")
				}
			})
		}
	}
}

func TestAttemptForgeryDeduplicationAndPreciseOutcomes(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	p := f.resolve(f.reference(state, "logical"))
	for _, mutate := range []func(*pluginapi.CachePolicyAttemptRequest){
		func(r *pluginapi.CachePolicyAttemptRequest) { r.Policy.TTL = "1h" },
		func(r *pluginapi.CachePolicyAttemptRequest) { r.Policy.Revision++ },
		func(r *pluginapi.CachePolicyAttemptRequest) { r.Policy.ContextEpoch = "changed" },
		func(r *pluginapi.CachePolicyAttemptRequest) { r.Policy.Kind = "warming" },
	} {
		r := pluginapi.CachePolicyAttemptRequest{Operation: "begin", RequestID: "logical", AttemptID: "forged", Capability: f.capability, Policy: p}
		mutate(&r)
		_, err := call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, r)
		checkError(t, err, 409, "snapshot_conflict")
	}
	attempt := f.begin(p, "logical", "physical")
	duplicate := attempt
	duplicate.StartedAt = time.Time{}
	_, err := call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, duplicate)
	checkError(t, err, 409, "attempt_reused")
	for _, operation := range []string{"validate", "finish"} {
		wrong := attempt
		wrong.Operation = operation
		wrong.StartedAt = wrong.StartedAt.Add(time.Nanosecond)
		if operation == "finish" {
			wrong.Outcome = "failed"
		}
		_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, wrong)
		checkError(t, err, 409, "snapshot_conflict")
	}
	foreign := attempt
	foreign.Operation = "validate"
	foreign.Policy.Scope.CallerScope = strings.Repeat("b", 64)
	_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, foreign)
	checkError(t, err, 403, "scope_denied")
	foreign = attempt
	foreign.Operation = "finish"
	foreign.Outcome = "failed"
	foreign.Capability = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))
	_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, foreign)
	checkError(t, err, 403, "scope_denied")
	checkError(t, f.finish(attempt, "succeeded", false), 400, "invalid")
	checkError(t, f.finish(attempt, "failed", true), 409, "attempt_unvalidated")
	if err = f.finish(attempt, "failed", false); err != nil {
		t.Fatal(err)
	}
	if err = f.finish(attempt, "failed", false); err != nil {
		t.Fatal(err)
	}
	checkError(t, f.finish(attempt, "canceled", false), 409, "outcome_conflict")
	f.clock.Add(int64(time.Second))
	retry := f.begin(p, "logical", "retry")
	f.validate(retry)
	if err = f.finish(retry, "canceled", true); err != nil {
		t.Fatal(err)
	}
	next := retry
	next.Operation, next.AttemptID, next.StartedAt = "begin", "after-cancel", time.Time{}
	_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, next)
	checkError(t, err, 409, "request_closing")
	if err = f.complete("logical", pluginapi.RequestCompletionCanceled); err != nil {
		t.Fatal(err)
	}
	if err = f.complete("logical", pluginapi.RequestCompletionCanceled); err != nil {
		t.Fatal(err)
	}
	checkError(t, f.complete("logical", pluginapi.RequestCompletionSucceeded), 409, "outcome_conflict")
}

func TestCancellationRetainsCredentialsUntilEveryAttemptStops(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	p := f.resolve(f.reference(state, "logical"))
	attempt := f.begin(p, "logical", "physical")
	checkError(t, f.complete("logical", pluginapi.RequestCompletionCanceled), 409, "inflight")
	if len(f.a.requests) != 1 || f.read().ActivePrimary != 1 {
		t.Fatal("early completion released private owner/guard")
	}
	next := attempt
	next.Operation, next.AttemptID, next.StartedAt = "begin", "late-retry", time.Time{}
	_, err := call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, next)
	checkError(t, err, 409, "request_closing")
	next = attempt
	next.Operation = "validate"
	_, err = call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, next)
	checkError(t, err, 409, "request_closing")
	canceled, cancel := context.WithCancel(testContext)
	cancel()
	attempt.Operation, attempt.Outcome = "finish", "canceled"
	if _, err = call[map[string]any](canceled, f.a, pluginabi.MethodCachePolicyAttempt, attempt); err != nil {
		t.Fatal("canceled request prevented transport-stopped cleanup", err)
	}
	if _, err = call[map[string]any](canceled, f.a, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "logical", Outcome: pluginapi.RequestCompletionCanceled}); err != nil {
		t.Fatal(err)
	}
	if len(f.a.requests) != 0 || f.read().ActivePrimary != 0 || f.read().ActiveAttempts != 0 {
		t.Fatal("settled cancellation retained guards")
	}
	f.resolve(f.reference(f.read(), "rejected-before-execution"))
	if err = f.complete("rejected-before-execution", pluginapi.RequestCompletionRejected); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err = f.db.QueryRow(`SELECT outcome FROM cache_policy_requests WHERE request_id='rejected-before-execution'`).Scan(&outcome); err != nil || outcome != "failed" {
		t.Fatal("rejection mapping", outcome, err)
	}
}

func TestRestartOrphansRequireExplicitStoppedRecovery(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	p := f.resolve(f.reference(state, "logical"))
	attempt := f.begin(p, "logical", "physical")
	f.validate(attempt)
	f.restart()
	f.clock.Add(int64(48 * time.Hour))
	if owned, err := f.a.CompleteRequest(testContext, pluginapi.RequestCompletion{RequestID: "logical", Outcome: pluginapi.RequestCompletionCanceled}); err != nil || owned {
		t.Fatal("restart adopted an unowned completion", owned, err)
	}
	for _, id := range []string{"logical", "new-logical"} {
		_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, f.reference(state, id))
		checkError(t, err, 409, "recovery_required")
	}
	checkError(t, f.finish(attempt, "succeeded", true), 409, "request_missing")
	items, err := f.a.Outstanding(testContext, f.scope, f.capability)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	pending := f.control("POST", f.operation("prepare", f.read(), "1h"), 200, "")
	f.control("POST", f.commit(pending, "after-recovery"), 409, "inflight")
	checkError(t, f.a.FinishStoppedRequest(testContext, f.scope, f.capability, "logical", cachepolicy.Succeeded), 400, "invalid")
	wrongCap := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))
	checkError(t, f.a.FinishStoppedRequest(testContext, f.scope, wrongCap, "logical", cachepolicy.Failed), 403, "scope_denied")
	// A trusted local controller has now independently confirmed all old dispatch stopped.
	if err = f.a.FinishStoppedRequest(testContext, f.scope, f.capability, "logical", cachepolicy.Failed); err != nil {
		t.Fatal(err)
	}
	if err = f.a.FinishStoppedRequest(testContext, f.scope, f.capability, "logical", cachepolicy.Failed); err != nil {
		t.Fatal(err)
	}
	items, err = f.a.Outstanding(testContext, f.scope, f.capability)
	if err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
	active := f.control("POST", f.commit(pending, "after-recovery"), 200, "")
	if active.Active.TTL != cachepolicy.TTL1h {
		t.Fatal(active)
	}
}

func TestStorageFailuresPreservePhysicalAndLogicalGuards(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	p := f.resolve(f.reference(state, "logical"))
	attempt := f.begin(p, "logical", "physical")
	f.validate(attempt)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_attempt BEFORE UPDATE ON cache_policy_attempts BEGIN SELECT RAISE(ABORT, 'private sqlite failure'); END`); err != nil {
		t.Fatal(err)
	}
	checkError(t, f.finish(attempt, "succeeded", true), 503, "unavailable")
	if f.read().ActiveAttempts != 1 || f.a.requests["logical"].attempts["physical"].outcome != "" {
		t.Fatal("failed finish consumed a guard")
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_attempt`); err != nil {
		t.Fatal(err)
	}
	if err := f.finish(attempt, "succeeded", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_request BEFORE UPDATE ON cache_policy_requests BEGIN SELECT RAISE(ABORT, 'private sqlite failure'); END`); err != nil {
		t.Fatal(err)
	}
	checkError(t, f.complete("logical", pluginapi.RequestCompletionSucceeded), 503, "unavailable")
	if f.read().ActivePrimary != 1 || f.a.requests["logical"] == nil {
		t.Fatal("failed logical finish dropped capability/guard")
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_request`); err != nil {
		t.Fatal(err)
	}
	if err := f.complete("logical", pluginapi.RequestCompletionSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE cache_policy_scopes SET state='corrupt'`); err != nil {
		t.Fatal(err)
	}
	_, err := call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, f.reference(state, "storage-broken"))
	checkError(t, err, 503, "unavailable")
}

func TestOpenNeverCreatesAndMissingStateCannotResolve(t *testing.T) {
	f := newFixture(t)
	emptyDB := testDB(t, filepath.Join(t.TempDir(), "lost.sqlite"))
	_, err := Open(testContext, emptyDB, f.opts)
	checkError(t, err, 503, "unavailable")
	var tables int
	if err = emptyDB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'cache_policy_%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal("Open initialized missing state", tables, err)
	}
	state := f.enroll()
	if _, err = f.db.Exec(`DELETE FROM cache_policy_scopes`); err != nil {
		t.Fatal(err)
	}
	_, err = call[pluginapi.CachePolicy](testContext, f.a, pluginabi.MethodCachePolicyResolve, f.reference(state, "lost"))
	checkError(t, err, 409, "state_missing")
	f.control("POST", f.prepareExisting(), 409, "state_missing")
}

func TestConcurrentAttemptsAndEarlyCompletion(t *testing.T) {
	f := newFixture(t)
	state := f.enroll()
	p := f.resolve(f.reference(state, "logical"))
	const count = 12
	var wg sync.WaitGroup
	requests := make([]pluginapi.CachePolicyAttemptRequest, count)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := pluginapi.CachePolicyAttemptRequest{Operation: "begin", RequestID: "logical", AttemptID: fmt.Sprintf("attempt-%d", i), Capability: f.capability, Policy: p}
			response, err := call[pluginapi.CachePolicyAttemptResponse](testContext, f.a, pluginabi.MethodCachePolicyAttempt, r)
			if err != nil {
				t.Error(err)
				return
			}
			r.StartedAt = response.StartedAt
			requests[i] = r
		}(i)
	}
	wg.Wait()
	if f.read().ActiveAttempts != count {
		t.Fatal("lost concurrent attempt guards")
	}
	checkError(t, f.complete("logical", pluginapi.RequestCompletionCanceled), 409, "inflight")
	for _, r := range requests {
		wg.Add(1)
		go func(r pluginapi.CachePolicyAttemptRequest) {
			defer wg.Done()
			if err := f.finish(r, "failed", false); err != nil {
				t.Error(err)
			}
		}(r)
	}
	wg.Wait()
	if err := f.complete("logical", pluginapi.RequestCompletionCanceled); err != nil {
		t.Fatal(err)
	}
	if f.read().ActiveAttempts != 0 || f.read().ActivePrimary != 0 {
		t.Fatal("concurrent settlement lost counters")
	}
}
