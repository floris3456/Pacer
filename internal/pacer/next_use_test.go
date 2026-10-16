package pacer

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const previewCapability = "private-preview-capability-for-this-test-chat-only"

func TestUsageInUseTracksOnlyThisChatsPrimaryRequests(t *testing.T) {
	f := previewFixture(t)
	readActive := func() []string {
		response := f.p.events(pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + previewCapability}}, Query: url.Values{"view": {"usage"}, "v": {"1"}, "model": {f.q.Route}}})
		var value struct {
			Active []string `json:"active"`
		}
		if response.StatusCode != 200 || json.Unmarshal(response.Body, &value) != nil {
			t.Fatal(response.StatusCode, string(response.Body))
		}
		return value.Active
	}
	if active := readActive(); len(active) != 0 {
		t.Fatal(active)
	}
	r := f.before(t, "active-primary", "claude-opus-5-5")
	if response := f.after(t, r, "a"); response.Terminate {
		t.Fatal(string(response.ResponseBody))
	}
	if active := readActive(); len(active) != 1 || active[0] != "a" {
		t.Fatal(active)
	}
	f.complete(t, r.RequestID)
	if active := readActive(); len(active) != 0 {
		t.Fatal(active)
	}
	// Auxiliary work and other chats must not light up this chat's IN USE marker.
	f.p.mu.Lock()
	f.p.requests["auxiliary"] = attempt{Key: f.binding(t).Key, Auth: "b", Claim: &tracker.Claim{Kind: "compaction"}}
	f.p.requests["other-chat"] = attempt{Key: "other", Auth: "b", Claim: &tracker.Claim{Kind: "primary"}}
	f.p.mu.Unlock()
	if active := readActive(); len(active) != 0 {
		t.Fatal(active)
	}
}

func previewFixture(t *testing.T) *routingFixture {
	t.Helper()
	f := newRoutingFixture(t, "on")
	f.q.Capability = tracker.Digest(previewCapability)
	f.seed(t, 5*time.Minute)
	r := f.before(t, "preview-source", "claude-opus-5-5")
	if choice := f.pick(t, r, "a", "b"); !choice.Handled || choice.AuthID != "a" {
		t.Fatal("warm chat not kept", choice)
	}
	f.complete(t, r.RequestID)
	return f
}

func previewUsage(t *testing.T, f *routingFixture) nextUseView {
	t.Helper()
	response := f.p.events(pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + previewCapability}}, Query: url.Values{"view": {"usage"}, "v": {"1"}, "model": {f.q.Route}}})
	var value struct {
		Next nextUseView `json:"next_use"`
	}
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &value) != nil {
		t.Fatal(response.StatusCode, string(response.Body))
	}
	return value.Next
}

func TestNextUseNamesTheColdMoveWhileWarmWithoutChangingBinding(t *testing.T) {
	f := previewFixture(t)
	before := f.binding(t)
	// Warm: the chat stays, but NEXT USE already names where it goes once cold.
	value := previewUsage(t, f)
	if value.Auth != "b" || value.Reason != routing.ColdMove || !value.Warm || value.Forced || value.ForceAuth != "" || value.Mode != "active" || value.ExpiresMS <= value.AtMS {
		t.Fatal(value)
	}
	// No model request occurs: the next UI poll sees that cache protection ended.
	f.clock.Add(int64(6 * time.Minute))
	if value = previewUsage(t, f); value.Auth != "b" || value.Reason != routing.ColdMove || value.Warm {
		t.Fatal(value)
	}
	if after := f.binding(t); after != before {
		t.Fatal("preview changed affinity or consumed checkpoint", before, after)
	}
	if _, active, err := f.p.chats.Get(before.Key); err != nil || active {
		t.Fatal("preview created an active flight", err)
	}
	// Staying is preferred within the band: an account as good as the best is no NEXT USE.
	f.p.mu.Lock()
	f.p.settings.Band = 30
	f.p.mu.Unlock()
	if value = previewUsage(t, f); value.Auth != "" || value.Reason != routing.ColdStay {
		t.Fatal(value)
	}
}

func TestForceUseSoonNearAFloorAndNowAtIt(t *testing.T) {
	f := previewFixture(t)
	short := func(left float64) {
		now := f.p.now()
		if err := f.p.observer.Observe(f.p.creds["a"].Account, "claude", []observation.Window{{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &left, Reset: now.Add(time.Hour), Usage: observation.Field{At: now.Add(time.Second), Source: "header"}, ResetEvidence: observation.Field{At: now.Add(time.Second), Source: "header", Quality: "absolute"}}}); err != nil {
			t.Fatal(err)
		}
		f.clock.Add(int64(time.Second))
	}
	// 5-hour floor 2 %, warning 5 points: 8 % left is not near, 6 % is.
	short(8)
	if value := previewUsage(t, f); value.ForceAuth != "" {
		t.Fatal(value)
	}
	short(6)
	if value := previewUsage(t, f); value.ForceAuth != "b" || value.ForceReason != "soon" || value.Auth != "b" || value.Forced {
		t.Fatal(value)
	}
	f.p.mu.Lock()
	f.p.settings.ForceWarning = 0
	f.p.mu.Unlock()
	if value := previewUsage(t, f); value.ForceAuth != "" {
		t.Fatal("warning 0 still warned", value)
	}
	short(2)
	if value := previewUsage(t, f); value.ForceAuth != "b" || value.ForceReason != "now" || value.Auth != "b" || !value.Forced {
		t.Fatal(value)
	}
}

// Pings and other generations of the chat on its model stay on its account,
// renew its warmth and appear in the decision trace.
func TestWarmingPingsStayWithTheChatAndKeepItWarm(t *testing.T) {
	f := newRoutingFixture(t, "on")
	f.seed(t, 0) // no cache evidence: warm for warm_minutes after the last request
	ping := func(id string, ids ...string) pluginapi.SchedulerPickResponse {
		t.Helper()
		f.q.Kind = "generate"
		defer func() { f.q.Kind = "primary" }()
		r := f.before(t, id, "claude-opus-5-5")
		choice := f.pick(t, r, ids...)
		auth := choice.AuthID
		if !choice.Handled {
			auth = ids[len(ids)-1]
		}
		if got := f.after(t, r, auth); got.Terminate {
			t.Fatal(string(got.ResponseBody))
		}
		f.complete(t, r.RequestID)
		return choice
	}
	before := f.binding(t)
	// CPA's own affinity would have picked b; the ping keeps the chat's account.
	for i := 0; i < 3; i++ {
		f.clock.Add(int64(50 * time.Minute))
		f.observe(t)
		if got := ping("ping-"+string(rune('0'+i)), "b", "a"); !got.Handled || got.AuthID != "a" {
			t.Fatal("ping left the chat's account", got)
		}
	}
	b := f.binding(t)
	if b.Account != before.Account || b.Generation != before.Generation || !b.LastUsed.After(before.LastUsed) {
		t.Fatal("ping rebound or did not renew", before, b)
	}
	// 150 minutes after its last step, still warm: the step stays.
	r := f.before(t, "after-pings", "claude-opus-5-5")
	if got := f.pick(t, r, "a", "b"); !got.Handled || got.AuthID != "a" || f.p.requests[r.RequestID].Decision.Reason != routing.Warm {
		t.Fatal("pinged chat moved", got)
	}
	f.after(t, r, "a")
	f.complete(t, r.RequestID)
	decisions, err := f.p.decisionView()
	if err != nil {
		t.Fatal(err)
	}
	pings := 0
	for _, d := range decisions {
		if d.Proposed.Reason == routing.WarmAuxiliary && d.ActualAuth == "a" && d.Outcome == "admitted" {
			pings++
		}
	}
	if pings != 3 {
		t.Fatal("pings missing from the decision trace", pings)
	}
	// Account not offered (cooling): CPA decides, and a ping elsewhere renews nothing.
	held := f.binding(t)
	f.clock.Add(int64(time.Minute))
	if got := ping("ping-elsewhere", "b"); got.Handled {
		t.Fatal("ping placed on another account", got)
	}
	if f.binding(t) != held {
		t.Fatal("ping on another account changed the binding")
	}
	// Another model reads another cache; a chat Pacer does not know is left to CPA.
	f.q.Kind = "generate"
	if f.pick(t, f.before(t, "other-model", "claude-haiku-4-5"), "a", "b").Handled {
		t.Fatal("other-model generation pinned")
	}
	f.q.Session = "unknown-chat"
	if f.pick(t, f.before(t, "unknown", "claude-opus-5-5"), "a", "b").Handled {
		t.Fatal("generation without a chat pinned")
	}
	// Routing off: the chat's pings still stay on its account (keep_account);
	// another model is CPA's.
	f.q.Session = "chat"
	f.p.cfg.Routing.Mode = "off"
	if got := f.pick(t, f.before(t, "off", "claude-opus-5-5"), "b", "a"); !got.Handled || got.AuthID != "a" {
		t.Fatal("off left the chat's ping to CPA", got)
	}
	if f.pick(t, f.before(t, "off-other-model", "claude-haiku-4-5"), "a", "b").Handled {
		t.Fatal("off pinned another model")
	}
}

func TestNextUseForcedExhaustionAndUnavailableCandidates(t *testing.T) {
	f := previewFixture(t)
	zero := 0.0
	now := f.p.now()
	if err := f.p.observer.Observe(f.p.creds["a"].Account, "claude", []observation.Window{{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &zero, Reset: now.Add(time.Hour), Usage: observation.Field{At: now.Add(time.Second), Source: "header"}, ResetEvidence: observation.Field{At: now.Add(time.Second), Source: "header", Quality: "absolute"}}}); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(time.Second))
	if value := previewUsage(t, f); value.Auth != "b" || value.Reason != "forced_move" || !value.Forced {
		t.Fatal(value)
	}
	// A credential removed/disabled by CPA discovery cannot remain NEXT USE.
	f.p.mu.Lock()
	delete(f.p.creds, "b")
	f.p.mu.Unlock()
	if value := previewUsage(t, f); value.Auth != "" {
		t.Fatal(value)
	}
}

func TestNextUseScopeExpiryAndMode(t *testing.T) {
	f := previewFixture(t)
	f.clock.Add(int64(6 * time.Minute))
	for _, query := range []url.Values{
		{"view": {"usage"}, "v": {"1"}, "model": {"another/model"}},
		{"view": {"usage"}, "v": {"1"}, "model": {f.q.Route}},
	} {
		response := f.p.events(pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + strings.Repeat("x", 48)}}, Query: query})
		if response.StatusCode != 401 || strings.Contains(string(response.Body), "next_use") {
			t.Fatal(response.StatusCode, string(response.Body))
		}
	}
	f.p.mu.Lock()
	f.p.cfg.Routing.Mode = "off"
	f.p.mu.Unlock()
	if value := previewUsage(t, f); value.Auth != "" || value.Reason != "routing_off" {
		t.Fatal(value)
	}
	f.p.mu.Lock()
	f.p.cfg.Routing.Mode = "on"
	f.p.mu.Unlock()
	f.clock.Add(int64(previewRetention))
	if value := previewUsage(t, f); value.Auth != "" || value.Reason != "unavailable" {
		t.Fatal(value)
	}
}
