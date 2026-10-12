package pacer

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func quotaLeft(t *testing.T, f *routingFixture, id string, left float64) {
	t.Helper()
	f.clock.Add(int64(time.Second))
	now := f.p.now()
	w := observation.Window{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &left,
		Reset: now.Add(96 * time.Hour), Usage: observation.Field{At: now, Source: "header"},
		ResetEvidence: observation.Field{At: now, Source: "header", Quality: "absolute"}}
	if _, err := f.p.credits.ObserveCredits(f.p.creds[id].Account, observation.Credits{State: "empty", Source: "api", ReadMS: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := f.p.observer.Observe(f.p.creds[id].Account, "claude", []observation.Window{w}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeQuotaSafetyPreservesExtraUsageAndUnknownCapacity(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	quotaLeft(t, f, "a", 0)
	windows, err := f.p.observer.ReadWindows(f.p.creds["a"].Account)
	if err != nil {
		t.Fatal(err)
	}
	now := f.p.now()
	for _, state := range []string{"available", "unlimited", "unknown", "empty"} {
		extra := observation.Credits{State: state, Source: "api", ReadMS: now.UnixMilli(), ExpiresMS: now.Add(time.Hour).UnixMilli()}
		if got := claudeUnavailable(windows, "claude-opus-5-5", now, extra, true); got != (state == "empty") {
			t.Fatal(state, got)
		}
		if state == "empty" {
			extra.ReadMS++
			if claudeUnavailable(windows, "claude-opus-5-5", now.Add(time.Second), extra, true) {
				t.Fatal("mixed new extra status with old quota")
			}
			extra.ReadMS = now.Add(-2 * time.Hour).UnixMilli()
			extra.ExpiresMS = now.UnixMilli()
			if claudeUnavailable(windows, "claude-opus-5-5", now, extra, true) {
				t.Fatal("stale extra capacity denied")
			}
		}
	}
}

func TestClaudeQuotaSafetyEvidence(t *testing.T) {
	now := time.Now()
	zero := 0.0
	w := observation.Window{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &zero,
		Reset: now.Add(time.Hour), Usage: observation.Field{At: now, Source: "header"}, ResetEvidence: observation.Field{At: now, Quality: "absolute"}}
	for _, tc := range []struct {
		name   string
		change func(*observation.Window)
		want   bool
	}{
		{"weekly empty", func(*observation.Window) {}, true},
		{"short empty", func(w *observation.Window) { w.Seconds = 18000 }, true},
		{"nonempty", func(w *observation.Window) { n := 0.1; w.Remaining = &n }, false},
		{"unknown", func(w *observation.Window) { w.Remaining = nil }, false},
		{"partial", func(w *observation.Window) { w.Reset = time.Time{} }, false},
		{"stale", func(w *observation.Window) { w.Usage.At = now.Add(-time.Hour) }, false},
		{"future", func(w *observation.Window) { w.Usage.At = now.Add(time.Second) }, false},
		{"reset elapsed", func(w *observation.Window) { w.Reset = now }, false},
		{"conflicting", func(w *observation.Window) { w.Conflict = true }, false},
		{"ambiguous", func(w *observation.Window) { w.Ambiguous = true }, false},
		{"dormant", func(w *observation.Window) { w.Dormant = true; w.StatusEvidence.At = now }, false},
		{"not applicable", func(w *observation.Window) { w.NotApplicable = true }, false},
		{"other model", func(w *observation.Window) { w.Scope = "sonnet" }, false},
		{"matching model", func(w *observation.Window) { w.Scope = "opus" }, true},
		{"codex credits", func(w *observation.Window) { w.Provider = "codex" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := w
			tc.change(&v)
			if got := claudeExhausted([]observation.Window{v}, "claude-opus-5-5", now); got != tc.want {
				t.Fatalf("exhausted=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaudeQuotaSafetySkipsEmptyDestinationBeforeCompaction(t *testing.T) {
	f := newRoutingFixture(t, "dry-run", false)
	f.seed(t, 5*time.Minute)
	before := f.binding(t)
	f.p.creds["c"] = credential{ID: "c", Index: "ic", Provider: "claude", Account: "account-c", Alias: "Claude C"}
	quotaLeft(t, f, "a", 0)
	quotaLeft(t, f, "b", 0)
	quotaLeft(t, f, "c", 50)
	r := f.before(t, "handoff", "claude-opus-5-5")
	if pick := f.pick(t, r, "a", "b", "c"); !pick.Handled || pick.AuthID != "c" {
		t.Fatal(pick)
	}
	// A stale/concurrent host selection cannot consume the existing checkpoint.
	rejected := f.after(t, r, "b")
	if !rejected.Terminate || rejected.StatusCode != 429 || rejected.ResponseHeaders.Get("X-Should-Retry") != "false" {
		t.Fatal(rejected)
	}
	var body struct{ Error struct{ Code string } }
	if json.Unmarshal(rejected.ResponseBody, &body) != nil || body.Error.Code != "quota_exhausted" {
		t.Fatal(string(rejected.ResponseBody))
	}
	if f.binding(t) != before {
		t.Fatal("empty account consumed checkpoint or changed binding")
	}
	requireCompact(t, f.after(t, r, "c"))
	f.complete(t, r.RequestID)
	f.q.Checkpoint = "portable-new"
	r = f.before(t, "resumed", "claude-opus-5-5")
	if pick := f.pick(t, r, "b", "c"); pick.AuthID != "c" {
		t.Fatal(pick)
	}
	if reply := f.after(t, r, "c"); reply.Terminate {
		t.Fatal(string(reply.ResponseBody))
	}
	if got := f.binding(t); got.Auth != "c" || got.Consumed != "portable-new" {
		t.Fatal(got)
	}
}

func TestClaudeQuotaSafetyAllEmptyAndReset(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	f.seed(t, time.Minute)
	before := f.binding(t)
	quotaLeft(t, f, "a", 0)
	quotaLeft(t, f, "b", 0)
	r := f.before(t, "empty", "claude-opus-5-5")
	if f.pick(t, r, "a", "b").Handled {
		t.Fatal("selected a known empty account")
	}
	for _, id := range []string{"a", "b"} {
		if got := f.after(t, r, id); !got.Terminate || got.StatusCode != 429 {
			t.Fatal(got)
		}
	}
	if f.binding(t) != before {
		t.Fatal("exhaustion changed binding")
	}
	quotaLeft(t, f, "a", 60)
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" {
		t.Fatal(got)
	}
	if got := f.after(t, r, "a"); got.Terminate {
		t.Fatal(got)
	}
}

func TestClaudeQuotaSafetyPreservesHostBoundaries(t *testing.T) {
	f := newRoutingFixture(t, "dry-run", false)
	f.seed(t, time.Minute)
	quotaLeft(t, f, "b", 0)
	f.p.creds["c"] = credential{ID: "c", Provider: "claude", Account: "c"}
	for _, tc := range []struct {
		name       string
		candidates []pluginapi.SchedulerAuthCandidate
		want       string
	}{
		{"healthy affinity below priority", []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}, {ID: "c", Provider: "claude", Priority: 10}}, "a"},
		{"tried or pinned out binding", []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "claude"}, {ID: "c", Provider: "claude"}}, "c"},
		{"only pinned empty account", []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "claude"}}, ""},
		{"weight zero", []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude", Attributes: map[string]string{"weight": "0"}}, {ID: "b", Provider: "claude"}, {ID: "c", Provider: "claude"}}, "c"},
		{"foreign provider", []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "claude"}}, ""},
		{"unknown credential", []pluginapi.SchedulerAuthCandidate{{ID: "unknown", Provider: "claude"}, {ID: "b", Provider: "claude"}}, ""},
		{"duplicate credential", []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "claude"}, {ID: "a", Provider: "claude"}, {ID: "b", Provider: "claude"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.before(t, tc.name, "claude-opus-5-5")
			pick := invoke(t, f.p, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{Provider: "claude", Model: r.Model, Options: pluginapi.SchedulerOptions{Headers: r.Headers}, Candidates: tc.candidates}).(pluginapi.SchedulerPickResponse)
			if pick.AuthID != tc.want {
				t.Fatal(pick)
			}
			f.complete(t, r.RequestID)
		})
	}
	// No affinity support is required for a new chat's availability guard.
	f.q.Session = "brand-new"
	f.p.Host = func(method string, _ any) (json.RawMessage, error) {
		return json.RawMessage(`{"status":"unsupported"}`), nil
	}
	r := f.before(t, "new", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); got.AuthID != "a" {
		t.Fatal(got)
	}
}

func TestClaudeQuotaSafetyReplacementPriorityAndCallerGrants(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	f.seed(t, time.Minute)
	quotaLeft(t, f, "a", 0)
	f.p.creds["c"] = credential{ID: "c", Provider: "claude", Account: "c"}
	r := f.before(t, "priority", "claude-opus-5-5")
	req := pluginapi.SchedulerPickRequest{Provider: "claude", Model: r.Model, Options: pluginapi.SchedulerOptions{Headers: r.Headers}, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "a", Provider: "claude", Priority: 100}, {ID: "b", Provider: "claude", Priority: 5}, {ID: "c", Provider: "claude", Priority: 10},
	}}
	pick := func() pluginapi.SchedulerPickResponse {
		return invoke(t, f.p, pluginabi.MethodSchedulerPick, req).(pluginapi.SchedulerPickResponse)
	}
	if got := pick(); got.AuthID != "c" {
		t.Fatal(got)
	}
	f.p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: "fixture-caller", AuthIDs: []string{"a", "b"}}}}
	if got := pick(); got.AuthID != "b" {
		t.Fatal("caller grant bypass", got)
	}
	if got := f.after(t, r, "c"); !got.Terminate || got.StatusCode != 403 {
		t.Fatal(got)
	}
	f.p.cfg.Enabled = false
	if got := pick(); got.Handled {
		t.Fatal("disabled plugin selected account", got)
	}
}

func TestClaudeQuotaSafetyDoesNotRerouteAuxiliaryRequests(t *testing.T) {
	f := newRoutingFixture(t, "dry-run", false)
	quotaLeft(t, f, "a", 0)
	for _, kind := range []string{"compaction", "title", "generate"} {
		f.q.Kind = kind
		r := f.before(t, kind, "claude-opus-5-5")
		if got := f.pick(t, r, "a", "b"); got.Handled {
			t.Fatal(kind, got)
		}
		if got := f.after(t, r, "a"); got.Terminate {
			t.Fatal("auxiliary must retain host-owned failover", kind, got)
		}
		f.complete(t, r.RequestID)
	}
}

func TestClaudeExtraCaptureKeepsSourceClock(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	quotaLeft(t, f, "a", 0)
	f.clock.Add(int64(10 * time.Second))
	at := f.p.now().Add(-time.Second)
	c := f.p.creds["a"]
	key := "Anthropic-Ratelimit-Unified-Overage-Status"
	h := http.Header{key: {"allowed"}}
	f.p.observeClaudeExtraCaptured(c, h, map[string]time.Time{key: at})
	got := f.p.readCredits(c.Account)
	if got.State != "available" || got.ReadMS != at.UnixMilli() || got.Source != "header" {
		t.Fatal(got)
	}
	f.clock.Add(int64(20 * time.Minute))
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1")
	f.p.observeClaudeExtraCaptured(c, h, map[string]time.Time{key: at, "Anthropic-Ratelimit-Unified-7d-Utilization": f.p.now()})
	if current := f.p.readCredits(c.Account); current != got {
		t.Fatal("late unrelated headers refreshed extra capacity", current)
	}
	f.p.observeClaudeExtraCaptured(c, http.Header{key: {"rejected"}}, nil)
	if current := f.p.readCredits(c.Account); current != got {
		t.Fatal("missing source clock became evidence", current)
	}
}
