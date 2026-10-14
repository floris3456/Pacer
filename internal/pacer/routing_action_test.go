package pacer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestRoutingActionRequiresTheExactCurrentCandidate(t *testing.T) {
	base := pluginapi.SchedulerPickRequest{Provider: "claude", Providers: []string{"claude"}, Model: "claude-opus-5-5", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude", Attributes: map[string]string{"unchanged": "yes"}}, {ID: "b", Provider: "claude"}}}
	known := []credential{{ID: "a", Provider: "claude", Account: "account-a"}, {ID: "b", Provider: "claude", Account: "account-b"}}
	d := routing.Decision{Handled: true, Auth: "b", Account: "account-b", Reason: "cold_move"}
	before, _ := json.Marshal(base)
	if got := routingAction(base, d, known); got != (pluginapi.SchedulerPickResponse{Handled: true, AuthID: "b"}) {
		t.Fatal(got)
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
			if got := routingAction(r, x, k); got != (pluginapi.SchedulerPickResponse{}) {
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
	if !routingAction(inferred, d, known).Handled {
		t.Fatal("single inferred provider refused")
	}
}

type routingFixture struct {
	p     *Plugin
	clock atomic.Int64
	q     tracker.Claim
}

// The fixture sets the mode directly. No worker or provider HTTP.
func newRoutingFixture(t *testing.T, mode string) *routingFixture {
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
	f.p.cfg.Routing.Mode = mode
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

func TestHandleRoutesOnlyWhenOn(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		handled bool
	}{{"off", false}, {"dry-run", false}, {"on", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newRoutingFixture(t, tc.mode)
			r := f.before(t, "new", "claude-opus-5-5")
			got := f.pick(t, r, "a", "b")
			if got.Handled != tc.handled || got.Handled && got.AuthID != "b" {
				t.Fatal(got)
			}
			state := invoke(t, f.p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/pacer/state"}).(pluginapi.ManagementResponse)
			var body struct {
				Routing struct {
					Mode string `json:"mode"`
				} `json:"routing"`
				Accounts []struct {
					ID   string   `json:"id"`
					Pace *float64 `json:"pace"`
				} `json:"accounts"`
			}
			if json.Unmarshal(state.Body, &body) != nil || state.StatusCode != 200 || body.Routing.Mode != tc.mode || len(body.Accounts) != 2 || body.Accounts[1].Pace == nil || *body.Accounts[1].Pace != 80.0/96 {
				t.Fatal(string(state.Body))
			}
		})
	}
}

func TestHandleGracefulColdMoveKeepsCheckpointAndForcedMoveRequiresCompaction(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "forced"}[forced], func(t *testing.T) {
			f := newRoutingFixture(t, "on")
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
			f := newRoutingFixture(t, "on")
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
	for _, mode := range []string{"off", "dry-run"} {
		f := newRoutingFixture(t, mode)
		f.seed(t, 5*time.Minute)
		f.clock.Add(int64(6 * time.Minute))
		f.observe(t)
		r := f.before(t, "fallback", "claude-opus-5-5")
		if f.pick(t, r, "a", "b").Handled {
			t.Fatal("closed gate acted")
		}
		requireCompact(t, f.after(t, r, "b")) // CPA's own move stays forced.
	}
}

func TestHandleWarmContextAliasAndAuxiliaryCoexist(t *testing.T) {
	f := newRoutingFixture(t, "on")
	f.seed(t, time.Hour)
	f.clock.Add(int64(6 * time.Minute))
	f.observe(t)
	before := f.binding(t)
	// Same declared route/checkpoint with a changed model alias remains the same chat.
	r := f.before(t, "warm-alias", "opus-public-alias")
	choice := f.pick(t, r, "a", "b")
	if !choice.Handled || choice.AuthID != "a" || f.p.requests[r.RequestID].Decision.Reason != routing.Warm {
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
		// A compaction on another model has no warm cache: it is placed by pace.
		// Titles and generations are left to CPA.
		if got := f.pick(t, r, "a", "b"); got.Handled != (kind == "compaction") || got.Handled && got.AuthID != "b" {
			t.Fatal("auxiliary placement", kind, got)
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
		f := newRoutingFixture(t, "on")
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

// Routing moments (docs/pace-routing.md): a chat moves only when its prompt
// cache is cold on every account, and then without compaction.
func TestHandleColdMomentsMoveWithoutCompaction(t *testing.T) {
	for _, moment := range []string{"unknown-warmth-expired", "new-checkpoint", "ttl-expired"} {
		t.Run(moment, func(t *testing.T) {
			f := newRoutingFixture(t, "on")
			switch moment {
			case "unknown-warmth-expired":
				f.seed(t, 0)
				f.clock.Add(int64(59 * time.Minute))
				f.observe(t)
				r := f.before(t, "within-hour", "claude-opus-5-5")
				if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" || f.p.requests[r.RequestID].Decision.Reason != routing.Warm {
					t.Fatal("unknown warmth moved before an hour", got)
				}
				f.complete(t, r.RequestID)
				f.clock.Add(int64(2 * time.Minute))
			case "new-checkpoint":
				f.seed(t, time.Hour)
				f.clock.Add(int64(time.Minute))
				f.q.Checkpoint = "committed-new"
			case "ttl-expired":
				f.seed(t, 5*time.Minute)
				f.clock.Add(int64(6 * time.Minute))
			}
			f.observe(t)
			before := f.binding(t)
			r := f.before(t, "cold", "claude-opus-5-5")
			got := f.pick(t, r, "a", "b")
			d := f.p.requests[r.RequestID].Decision
			if !got.Handled || got.AuthID != "b" || d.Reason != routing.ColdMove || d.Forced {
				t.Fatal(got, d)
			}
			if after := f.after(t, r, "b"); after.Terminate {
				t.Fatal("cold move asked for compaction", string(after.ResponseBody))
			}
			if b := f.binding(t); b.Account != f.p.creds["b"].Account || b.Generation != before.Generation+1 {
				t.Fatal("binding not moved", b)
			}
		})
	}
}

func TestHandleCompactionOnTheChatModelStaysOnItsAccount(t *testing.T) {
	f := newRoutingFixture(t, "on")
	f.seed(t, 5*time.Minute)
	f.clock.Add(int64(6 * time.Minute)) // cold: a primary request would move
	f.observe(t)
	before := f.binding(t)
	f.q.Kind = "compaction"
	r := f.before(t, "same-model", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatal("same-model compaction left the chat's cache", got)
	}
	f.complete(t, r.RequestID)
	r = f.before(t, "other-model", "claude-haiku-4-5")
	got := f.pick(t, r, "a", "b")
	if d := f.p.requests[r.RequestID].Decision; !got.Handled || got.AuthID != "b" || d.Reason != routing.ColdMove {
		t.Fatal("other-model compaction not placed by pace", got, d)
	}
	if after := f.after(t, r, "b"); after.Terminate {
		t.Fatal(string(after.ResponseBody))
	}
	if f.binding(t) != before {
		t.Fatal("compaction rebound the chat")
	}
	// A compaction for a chat Pacer has never seen is left to CPA.
	f.q.Session = "unknown-chat"
	if f.pick(t, f.before(t, "unknown", "claude-haiku-4-5"), "a", "b").Handled {
		t.Fatal("compaction without a chat routed")
	}
}

func TestDecisionTraceRecordsEveryCandidate(t *testing.T) {
	f := newRoutingFixture(t, "on")
	r := f.before(t, "new", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "b" {
		t.Fatal(got)
	}
	f.after(t, r, "b")
	decisions, err := f.p.decisionView()
	if err != nil || len(decisions) != 1 {
		t.Fatal(decisions, err)
	}
	d := decisions[0]
	if d.Outcome != "admitted" || d.Proposed.Reason != routing.NewChat || len(d.Proposed.Candidates) != 2 {
		t.Fatalf("%+v", d)
	}
	for _, c := range d.Proposed.Candidates {
		if !c.Known || !c.Eligible || c.Pace <= 0 || len(c.Budgets) != 2 {
			t.Fatalf("candidate inputs missing: %+v", c)
		}
	}
	// Off records nothing.
	f.p.cfg.Routing.Mode = "off"
	f.q.Session = "other"
	r = f.before(t, "off", "claude-opus-5-5")
	f.pick(t, r, "a", "b")
	f.after(t, r, "a")
	if decisions, _ = f.p.decisionView(); len(decisions) != 1 {
		t.Fatal("off recorded a decision")
	}
}

// A dormant 5-hour window (reset, not started) is a usable, full meter.
func TestHandleDormantWindowIsAvailable(t *testing.T) {
	f := newRoutingFixture(t, "on")
	f.clock.Add(int64(time.Minute)) // a newer meter read than the fixture's
	now := f.p.now()
	full, weekly := 100.0, 90.0
	windows := []observation.Window{
		{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &full, Dormant: true, Usage: observation.Field{At: now, Source: "api"}, StatusEvidence: observation.Field{At: now, Source: "api", Quality: "measured", Raw: "null"}},
		{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &weekly, Reset: now.Add(96 * time.Hour), Usage: observation.Field{At: now, Source: "api"}, ResetEvidence: observation.Field{At: now, Source: "api", Quality: "absolute"}},
	}
	if err := f.p.observer.Observe(f.p.creds["a"].Account, "claude", windows); err != nil {
		t.Fatal(err)
	}
	r := f.before(t, "new", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatal("dormant account excluded", got, f.p.requests[r.RequestID].Decision)
	}
}

// CPA reports affinity "unsupported" whenever a plugin scheduler is registered
// (conductor_selection.go LookupSessionAffinity), so every new chat reads that
// way in production. It is placed by pace; only an ambiguous answer or a failed
// lookup leaves it to CPA.
func TestHandleNewChatWhenCPAReportsAffinityUnsupported(t *testing.T) {
	for _, status := range []string{"unsupported", "unbound", "ambiguous", "error"} {
		t.Run(status, func(t *testing.T) {
			f := newRoutingFixture(t, "on")
			f.p.Host = func(method string, input any) (json.RawMessage, error) {
				if method != "host.affinity.lookup" {
					return testHost(method, input)
				}
				if status == "error" {
					return nil, errors.New("host unavailable")
				}
				return json.Marshal(pluginapi.HostAffinityLookupResponse{Status: status})
			}
			// A session header makes Pacer ask CPA, as OpenCode's requests do.
			raw, _ := json.Marshal(f.q)
			r := pluginapi.RequestInterceptRequest{RequestID: "new", Model: "claude-opus-5-5", Headers: http.Header{controlHeader: {base64.RawURLEncoding.EncodeToString(raw)}, "X-Session-Id": {"new-" + status}}, Metadata: map[string]any{"caller_scope": "fixture-caller"}}
			before := invoke(t, f.p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
			for k, v := range before.Headers {
				r.Headers[k] = v
			}
			if f.p.requests["new"].Session == "" {
				t.Fatal("fixture: no session")
			}
			got := f.pick(t, r, "a", "b")
			placed := status == "unsupported" || status == "unbound"
			if got.Handled != placed || placed && got.AuthID != "b" {
				t.Fatal(got)
			}
		})
	}
}

// A warm chat whose account CPA leaves out although its meters are fine (an
// upstream error in this request, a transient-error cooldown) is refused as
// retryable instead of moved, up to the retry setting; then it is moved.
func TestWarmChatWaitsForItsBrieflyUnavailableAccount(t *testing.T) {
	f := newRoutingFixture(t, "on")
	f.seed(t, time.Hour) // warm on a for an hour
	key := tracker.Key("fixture-caller", "claude", f.q)
	r := f.before(t, "retry", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" || f.p.requests[r.RequestID].Decision.Reason != "warm" {
		t.Fatal(got)
	}
	// CPA retries the same request without a; then the client retries twice more.
	for i := 1; i <= 3; i++ {
		got := f.pick(t, r, "b")
		if !got.Handled || !got.Reject || got.RejectCode != "auth_unavailable" || got.AuthID != "" || f.p.strikes[key].n != i {
			t.Fatal(i, got)
		}
	}
	decisions, err := f.p.decisionView()
	if err != nil || len(decisions) == 0 || decisions[0].Outcome != TransientWait || decisions[0].Proposed.Unoffered != true {
		t.Fatal("transient wait not traced", decisions)
	}
	// Past the setting: the chat is moved, compacting first.
	got := f.pick(t, r, "b")
	if !got.Handled || got.Reject || got.AuthID != "b" || !f.p.requests[r.RequestID].Decision.Forced {
		t.Fatal(got)
	}
	requireCompact(t, f.after(t, r, "b"))
	f.complete(t, r.RequestID)

	// a recovers within the retries: the chat stays, and its strikes end.
	f2 := newRoutingFixture(t, "on")
	f2.seed(t, time.Hour)
	r = f2.before(t, "blip", "claude-opus-5-5")
	if got := f2.pick(t, r, "b"); !got.Reject {
		t.Fatal(got)
	}
	if got := f2.pick(t, r, "a", "b"); got.AuthID != "a" {
		t.Fatal(got)
	}
	if after := f2.after(t, r, "a"); after.Terminate || f2.binding(t).Account != f2.p.creds["a"].Account {
		t.Fatal("blip moved the chat", string(after.ResponseBody))
	}
	// Admission precedes the upstream call: only a succeeded request ends strikes.
	if len(f2.p.strikes) != 1 {
		t.Fatal("admission cleared strikes")
	}
	f2.complete(t, r.RequestID)
	if len(f2.p.strikes) != 0 {
		t.Fatal("success kept strikes")
	}

	// A lasting outage: every attempt on a is admitted, then fails upstream.
	f3 := newRoutingFixture(t, "on")
	f3.seed(t, time.Hour)
	s := f3.p.settings
	s.TransientRetries = 1
	if err := f3.p.setSettings(s); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"hold", "move"} {
		r := f3.before(t, "outage-"+want, "claude-opus-5-5")
		if got := f3.pick(t, r, "a", "b"); got.AuthID != "a" {
			t.Fatal(i, got)
		}
		if after := f3.after(t, r, "a"); after.Terminate {
			t.Fatal(string(after.ResponseBody))
		}
		got := f3.pick(t, r, "b") // a's upstream answered 502
		if (want == "hold") != got.Reject || (want == "move" && got.AuthID != "b") {
			t.Fatal(want, got)
		}
		invoke(t, f3.p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: r.RequestID, Outcome: pluginapi.RequestCompletionFailed})
	}
}

func TestTransientWaitNeedsUsableMetersAWarmChatAndRetries(t *testing.T) {
	for _, tc := range []string{"floor", "cold", "zero-retries", "expired"} {
		t.Run(tc, func(t *testing.T) {
			f := newRoutingFixture(t, "on")
			f.seed(t, time.Hour)
			switch tc {
			case "floor":
				left, short := 30.0, 1.0
				now := f.p.now()
				if err := f.p.observer.Observe(f.p.creds["a"].Account, "claude", []observation.Window{
					{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &short, Reset: now.Add(3 * time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}},
					{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &left, Reset: now.Add(96 * time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}},
				}); err != nil {
					t.Fatal(err)
				}
			case "cold":
				f.clock.Add(int64(2 * time.Hour))
				f.observe(t)
			case "zero-retries":
				s := f.p.settings
				s.TransientRetries = 0
				if err := f.p.setSettings(s); err != nil {
					t.Fatal(err)
				}
			case "expired":
				r := f.before(t, "first", "claude-opus-5-5")
				for i := 0; i < 3; i++ {
					if !f.pick(t, r, "b").Reject {
						t.Fatal("not held")
					}
				}
				f.clock.Add(int64(transientWindow + time.Second))
				f.observe(t)
				if got := f.pick(t, r, "b"); !got.Reject {
					t.Fatal("expired strikes still counted", got)
				}
				return
			}
			r := f.before(t, "move", "claude-opus-5-5")
			if got := f.pick(t, r, "b"); !got.Handled || got.Reject || got.AuthID != "b" {
				t.Fatal(got)
			}
		})
	}
}

// The forced move's 409 names the configured summary models, in order.
func TestCompactRefusalCarriesTheCompactionModels(t *testing.T) {
	f := newRoutingFixture(t, "on")
	f.seed(t, 5*time.Minute)
	f.clock.Add(int64(6 * time.Minute))
	f.observe(t)
	s := f.p.settings
	s.CompactionModels = []string{"claude-sonnet-4-6", "gemini-3.8-flash-high"}
	if err := f.p.setSettings(s); err != nil {
		t.Fatal(err)
	}
	r := f.before(t, "move", "claude-opus-5-5")
	f.pick(t, r, "b")
	after := f.after(t, r, "b")
	requireCompact(t, after)
	var body struct {
		Error struct {
			Models []string `json:"compaction_models"`
		} `json:"error"`
	}
	if json.Unmarshal(after.ResponseBody, &body) != nil || strings.Join(body.Error.Models, ",") != "claude-sonnet-4-6,gemini-3.8-flash-high" {
		t.Fatal(string(after.ResponseBody))
	}
	// compact_on_switch off on the page: the move is admitted without compaction.
	f.complete(t, r.RequestID)
	s.CompactOnSwitch = false
	if err := f.p.setSettings(s); err != nil {
		t.Fatal(err)
	}
	r = f.before(t, "move-2", "claude-opus-5-5")
	f.pick(t, r, "b")
	if after = f.after(t, r, "b"); after.Terminate {
		t.Fatal(string(after.ResponseBody))
	}
}
