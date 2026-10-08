package pacer

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRoutingActionRequiresBothGatesAndExactCurrentCandidate(t *testing.T) {
	base := pluginapi.SchedulerPickRequest{Provider: "claude", Providers: []string{"claude"}, Model: "claude-opus-5-5", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude", Attributes: map[string]string{"unchanged": "yes"}}, {ID: "b", Provider: "claude"}}}
	known := []credential{{ID: "a", Provider: "claude", Account: "account-a"}, {ID: "b", Provider: "claude", Account: "account-b"}}
	d := routing.Decision{Handled: true, Auth: "b", Account: "account-b", Reason: "cold_move"}
	before, _ := json.Marshal(base)
	for _, mode := range []string{"", "off", "dry-run", "active", "ACTIVE", "unknown"} {
		for _, gate := range []bool{false, true} {
			got := routingAction(base, mode, gate, d, known)
			want := pluginapi.SchedulerPickResponse{}
			if mode == "active" && gate {
				want = pluginapi.SchedulerPickResponse{Handled: true, AuthID: "b"}
			}
			if got != want {
				t.Fatalf("mode=%q eligible=%v: %+v", mode, gate, got)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*pluginapi.SchedulerPickRequest, *routing.Decision, *[]credential)
	}{
		{"no decision", func(_ *pluginapi.SchedulerPickRequest, d *routing.Decision, _ *[]credential) { d.Handled = false }},
		{"no auth", func(_ *pluginapi.SchedulerPickRequest, d *routing.Decision, _ *[]credential) { d.Auth = "" }},
		{"no account", func(_ *pluginapi.SchedulerPickRequest, d *routing.Decision, _ *[]credential) { d.Account = "" }},
		{"cooled target", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) {
			r.Candidates = r.Candidates[:1]
		}},
		{"no candidates", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) { r.Candidates = nil }},
		{"no model", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) { r.Model = "" }},
		{"unconfigured provider", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) { r.Provider = "openai" }},
		{"mixed providers", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) {
			r.Providers = []string{"claude", "codex"}
		}},
		{"mixed candidates", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) {
			r.Candidates[0].Provider = "codex"
		}},
		{"unknown candidate", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) {
			r.Candidates[0].ID = "unknown"
		}},
		{"duplicate candidate", func(r *pluginapi.SchedulerPickRequest, _ *routing.Decision, _ *[]credential) {
			r.Candidates[0] = r.Candidates[1]
		}},
		{"missing discovery", func(_ *pluginapi.SchedulerPickRequest, _ *routing.Decision, k *[]credential) { *k = (*k)[:1] }},
		{"changed account", func(_ *pluginapi.SchedulerPickRequest, d *routing.Decision, _ *[]credential) { d.Account = "unrelated" }},
		{"unknown account", func(_ *pluginapi.SchedulerPickRequest, _ *routing.Decision, k *[]credential) { (*k)[1].Account = "" }},
		{"duplicate discovery", func(_ *pluginapi.SchedulerPickRequest, _ *routing.Decision, k *[]credential) {
			*k = append(*k, (*k)[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r pluginapi.SchedulerPickRequest
			_ = json.Unmarshal(before, &r)
			x, k := d, append([]credential(nil), known...)
			tc.change(&r, &x, &k)
			if got := routingAction(r, "active", true, x, k); got != (pluginapi.SchedulerPickResponse{}) {
				t.Fatal(got)
			}
		})
	}
	after, _ := json.Marshal(base)
	if string(before) != string(after) || known[1].Account != "account-b" {
		t.Fatal("mutated host candidates")
	}
	inferred := base
	inferred.Provider = ""
	if !routingAction(inferred, "active", true, d, known).Handled {
		t.Fatal("single inferred provider refused")
	}
}

type routingFixture struct {
	p     *Plugin
	clock atomic.Int64
	q     tracker.Claim
}

// Active injection exists ONLY in this _test.go fixture. Runtime YAML, management,
// durable settings and New never provide eligibility. No worker or provider HTTP.
func newRoutingFixture(t *testing.T, mode string, eligible bool) *routingFixture {
	t.Helper()
	f := &routingFixture{q: tracker.Claim{Protocol: 1, Installation: "fixture", Session: "chat", Route: "claude/opus-alias", Request: "client-request", Kind: "primary", Capability: tracker.Digest("private-test-capability"), Checkpoint: "committed-old", Auto: true, CompactOnSwitch: true}}
	f.clock.Store(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).UnixNano())
	f.p = New(testHost)
	f.p.now = func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + filepath.Join(t.TempDir(), "routing.sqlite") + "\ncompact_on_switch: true\nrouting:\n  mode: off\n")})
	if _, err := f.p.configure(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.p.Close)
	f.p.discover()
	f.p.cfg.Routing.Mode, f.p.routingEligible = mode, eligible
	f.observe(t)
	return f
}

func (f *routingFixture) observe(t *testing.T) {
	t.Helper()
	for id, left := range map[string]float64{"a": 30, "b": 80} {
		short := 80.0
		now := f.p.now()
		windows := []observation.Window{
			{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &short, Reset: now.Add(3 * time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}},
			{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &left, Reset: now.Add(96 * time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}},
		}
		if err := f.p.observer.Observe(f.p.creds[id].Account, "claude", windows); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *routingFixture) before(t *testing.T, id, model string) pluginapi.RequestInterceptRequest {
	t.Helper()
	raw, _ := json.Marshal(f.q)
	r := pluginapi.RequestInterceptRequest{RequestID: id, Model: model, Headers: http.Header{controlHeader: {base64.RawURLEncoding.EncodeToString(raw)}}, Metadata: map[string]any{"caller_scope": "fixture-caller"}}
	before := invoke(t, f.p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
	if before.Terminate {
		t.Fatal(string(before.ResponseBody))
	}
	for k, v := range before.Headers {
		r.Headers[k] = v
	}
	return r
}

func (f *routingFixture) pick(t *testing.T, r pluginapi.RequestInterceptRequest, ids ...string) pluginapi.SchedulerPickResponse {
	t.Helper()
	req := pluginapi.SchedulerPickRequest{Provider: "claude", Model: r.Model, Options: pluginapi.SchedulerOptions{Headers: r.Headers}}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude"})
	}
	// The same JSON/native method path used by cmd/plugin's C ABI entrypoint.
	result := invoke(t, f.p, pluginabi.MethodSchedulerPick, req)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var choice pluginapi.SchedulerPickResponse
	if err = json.Unmarshal(raw, &choice); err != nil {
		t.Fatal(err)
	}
	return choice
}

func (f *routingFixture) after(t *testing.T, r pluginapi.RequestInterceptRequest, auth string) pluginapi.RequestInterceptResponse {
	t.Helper()
	r.Metadata["selected_auth_id"] = auth
	return invoke(t, f.p, pluginabi.MethodRequestInterceptAfter, r).(pluginapi.RequestInterceptResponse)
}

func (f *routingFixture) complete(t *testing.T, id string) {
	t.Helper()
	invoke(t, f.p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: id, Outcome: pluginapi.RequestCompletionSucceeded})
}

func (f *routingFixture) binding(t *testing.T) tracker.Binding {
	t.Helper()
	b, _, err := f.p.chats.Get(tracker.Key("fixture-caller", "claude", f.q))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *routingFixture) seed(t *testing.T, ttl time.Duration) {
	t.Helper()
	r := f.before(t, "initial", "claude-opus-5-5")
	if got := f.after(t, r, "a"); got.Terminate {
		t.Fatal(string(got.ResponseBody))
	}
	if ttl > 0 {
		if err := f.p.chats.CacheActivityForAuth(r.RequestID, "a", f.p.now(), true, true, ttl); err != nil {
			t.Fatal(err)
		}
	}
	f.complete(t, r.RequestID)
}

func requireCompact(t *testing.T, r pluginapi.RequestInterceptResponse) {
	t.Helper()
	if !r.Terminate || r.StatusCode != 409 || !strings.Contains(string(r.ResponseBody), "quota_compact_required") {
		t.Fatalf("expected forced-move 409: %+v %s", r, r.ResponseBody)
	}
}

func TestHandleActiveRequiresPrivateEligibilityAndRuntimeGatesStayClosed(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		gate    bool
		handled bool
	}{{"off", false, false}, {"off", true, false}, {"dry-run", true, false}, {"active", false, false}, {"active", true, true}} {
		t.Run(tc.mode+"/"+map[bool]string{false: "ineligible", true: "eligible"}[tc.gate], func(t *testing.T) {
			f := newRoutingFixture(t, tc.mode, tc.gate)
			r := f.before(t, "new", "claude-opus-5-5")
			got := f.pick(t, r, "a", "b")
			if got.Handled != tc.handled || got.Handled && got.AuthID != "b" {
				t.Fatal(got)
			}
			// Injected active mode is not rollout evidence for management.
			state := invoke(t, f.p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/pacer/state"}).(pluginapi.ManagementResponse)
			var body struct {
				Routing struct {
					CanActivate bool `json:"can_activate"`
				} `json:"routing"`
			}
			if json.Unmarshal(state.Body, &body) != nil || state.StatusCode != 200 || body.Routing.CanActivate {
				t.Fatal(string(state.Body))
			}
			patch := invoke(t, f.p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"active"}`)}).(pluginapi.ManagementResponse)
			if patch.StatusCode != 409 || f.p.setMode("active") == nil {
				t.Fatal("runtime activation gate opened")
			}
		})
	}
	for _, yaml := range []string{"routing:\n  mode: active\n", "routing_eligible: true\n", "routing:\n  eligible: true\n"} {
		p := New(nil)
		if p.routingEligible {
			t.Fatal("New granted rollout authorization")
		}
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("state_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\n" + yaml)})
		_, err := p.Handle(pluginabi.MethodPluginRegister, raw)
		p.Close()
		if err == nil {
			t.Fatal("runtime YAML allowed test injection", yaml)
		}
	}
}

func TestHandleGracefulColdMoveKeepsCheckpointAndForcedMoveRequiresCompaction(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "forced"}[forced], func(t *testing.T) {
			f := newRoutingFixture(t, "active", true)
			f.seed(t, 5*time.Minute)
			before := f.binding(t)
			f.clock.Add(int64(6 * time.Minute))
			f.observe(t)
			r := f.before(t, "move", "claude-opus-5-5")
			ids := []string{"a", "b"}
			if forced {
				ids = []string{"b"}
			} // CPA's cooled/exhausted credential is no longer offered.
			choice := f.pick(t, r, ids...)
			if !choice.Handled || choice.AuthID != "b" || f.p.requests[r.RequestID].Decision.Forced != forced {
				t.Fatal(choice)
			}
			after := f.after(t, r, choice.AuthID)
			if forced {
				requireCompact(t, after)
				if f.binding(t) != before {
					t.Fatal("refusal changed old binding/checkpoint")
				}
				f.complete(t, r.RequestID)
				f.q.Checkpoint = "committed-new"
				r = f.before(t, "after-compaction", r.Model)
				choice = f.pick(t, r, ids...)
				after = f.after(t, r, choice.AuthID)
			}
			if after.Terminate {
				t.Fatal(string(after.ResponseBody))
			}
			b := f.binding(t)
			if b.Account != f.p.creds["b"].Account || b.Generation != before.Generation+1 || b.Consumed != f.q.Checkpoint {
				t.Fatal("move lost checkpoint/generation", b)
			}
			f.complete(t, r.RequestID)
			// Late successful old-account evidence cannot refresh the new account.
			if err := f.p.chats.CacheActivityForAuth("initial", "a", f.p.now(), true, true, time.Hour); err != nil {
				t.Fatal(err)
			}
			if f.binding(t) != b {
				t.Fatal("late old-account activity warmed the moved binding")
			}
		})
	}
}

func TestHandleHostDeviationAndRetryKeepForcedContract(t *testing.T) {
	for _, deviation := range []string{"different-auth-alias", "different-model", "retry-no-candidates", "retry-mixed", "mode-off", "disabled"} {
		t.Run(deviation, func(t *testing.T) {
			f := newRoutingFixture(t, "active", true)
			f.seed(t, 5*time.Minute)
			f.clock.Add(int64(6 * time.Minute))
			f.observe(t)
			alias := f.p.creds["b"]
			alias.ID = "b-alias"
			f.p.creds[alias.ID] = alias
			r := f.before(t, "move", "claude-opus-5-5")
			if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "b" {
				t.Fatal(got)
			}
			auth := "b"
			switch deviation {
			case "different-auth-alias":
				auth = "b-alias"
			case "different-model":
				r.Model = "claude-sonnet-4-6"
			case "retry-no-candidates":
				if f.pick(t, r).Handled {
					t.Fatal("empty retry handled")
				}
			case "retry-mixed":
				got := invoke(t, f.p, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{Provider: "claude", Options: pluginapi.SchedulerOptions{Headers: r.Headers}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "codex"}}}).(pluginapi.SchedulerPickResponse)
				if got.Handled {
					t.Fatal(got)
				}
			case "mode-off":
				f.p.cfg.Routing.Mode = "off"
				if f.pick(t, r, "a", "b").Handled {
					t.Fatal("off acted")
				}
			case "disabled":
				f.p.cfg.Enabled = false
				if f.pick(t, r, "a", "b").Handled {
					t.Fatal("disabled acted")
				}
			}
			requireCompact(t, f.after(t, r, auth))
		})
	}
	for _, mode := range []string{"off", "dry-run", "active"} {
		f := newRoutingFixture(t, mode, false)
		f.seed(t, 5*time.Minute)
		f.clock.Add(int64(6 * time.Minute))
		f.observe(t)
		r := f.before(t, "fallback", "claude-opus-5-5")
		if f.pick(t, r, "a", "b").Handled {
			t.Fatal("closed gate acted")
		}
		requireCompact(t, f.after(t, r, "b")) // An advisory cold_move is never permission.
	}
}

func TestHandleWarmContextAliasAndAuxiliaryCoexist(t *testing.T) {
	f := newRoutingFixture(t, "active", true)
	f.seed(t, time.Hour)
	f.clock.Add(int64(6 * time.Minute))
	f.observe(t)
	before := f.binding(t)
	// Same declared route/checkpoint with a changed model alias remains the same chat.
	r := f.before(t, "warm-alias", "opus-public-alias")
	choice := f.pick(t, r, "a", "b")
	if !choice.Handled || choice.AuthID != "a" || f.p.requests[r.RequestID].Decision.Reason != "protected" {
		t.Fatal(choice)
	}
	if got := f.after(t, r, "a"); got.Terminate {
		t.Fatal(string(got.ResponseBody))
	}
	f.complete(t, r.RequestID)
	// Successful same-prefix activity can arrive after completion and extend 1h warmth.
	if err := f.p.chats.CacheActivityForAuth(r.RequestID, "a", f.p.now(), true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	warm := f.binding(t)
	if warm.Account != before.Account || warm.Consumed != before.Consumed || !warm.ProtectedUntil.After(before.ProtectedUntil) {
		t.Fatal(warm)
	}
	for _, kind := range []string{"compaction", "title", "generate"} {
		f.q.Kind = kind
		r := f.before(t, kind, "summary-model")
		if f.pick(t, r, "a", "b").Handled {
			t.Fatal("auxiliary routed", kind)
		}
		if got := f.after(t, r, "b"); got.Terminate {
			t.Fatal(string(got.ResponseBody))
		}
		f.complete(t, r.RequestID)
		if err := f.p.chats.CacheActivityForAuth(r.RequestID, "b", f.p.now(), true, true, time.Hour); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.binding(t), warm) {
			t.Fatal("auxiliary rebound or warmed the primary context", kind)
		}
	}
	f.q.Kind = "primary"
	r = f.before(t, "still-warm", "opus-public-alias")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatal(got)
	}
}

func TestHandleUnknownWarmthAndInflightBindingsStay(t *testing.T) {
	for _, inflight := range []bool{false, true} {
		f := newRoutingFixture(t, "active", true)
		ttl := time.Duration(0)
		if inflight {
			ttl = 5 * time.Minute
		}
		f.seed(t, ttl)
		f.clock.Add(int64(6 * time.Minute))
		f.observe(t)
		if inflight {
			r := f.before(t, "still-streaming", "claude-opus-5-5")
			if got := f.after(t, r, "a"); got.Terminate {
				t.Fatal(string(got.ResponseBody))
			}
		}
		r := f.before(t, "next", "claude-opus-5-5")
		if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" {
			t.Fatal("unknown/inflight binding moved", got)
		}
	}
}

func TestSelectionIntentRequiresTheWholeEmittedProposal(t *testing.T) {
	c := credential{ID: "b", Account: "account-b", Provider: "claude", Alias: "B"}
	base := attempt{Key: "scoped-chat", Model: "model", PreviousAccount: "account-a", RoutingProvider: "claude",
		Decision:      routing.Decision{Handled: true, Auth: c.ID, Account: c.Account, Reason: "cold_move"},
		RoutingChoice: pluginapi.SchedulerPickResponse{Handled: true, AuthID: c.ID}}
	if selectionIntent(base, c, "model").Forced {
		t.Fatal("exact emitted cold move remained forced")
	}
	for _, mutate := range []func(*attempt, *credential){
		func(a *attempt, _ *credential) { a.Decision.Forced = true },
		func(a *attempt, _ *credential) { a.Decision.Handled = false },
		func(a *attempt, _ *credential) { a.Decision.Reason = "new_chat" },
		func(a *attempt, _ *credential) { a.RoutingChoice.Handled = false },
		func(a *attempt, _ *credential) { a.RoutingChoice.Reject = true },
		func(a *attempt, _ *credential) { a.RoutingChoice.DelegateBuiltin = "fill-first" },
		func(a *attempt, _ *credential) { a.PreviousAccount = "" },
		func(a *attempt, _ *credential) { a.Key = "" },
		func(a *attempt, _ *credential) { a.Claim = &tracker.Claim{Kind: "title"} },
		func(_ *attempt, c *credential) { c.Account = "replacement-credential-account" },
		func(_ *attempt, c *credential) { c.Provider = "codex" },
	} {
		a, actual := base, c
		mutate(&a, &actual)
		if !selectionIntent(a, actual, "model").Forced {
			t.Fatal("unmatched selection lost forced contract", a, actual)
		}
	}
}
