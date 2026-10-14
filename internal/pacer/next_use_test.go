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

func TestNextUseReevaluatesColdCacheWithoutChangingBinding(t *testing.T) {
	f := previewFixture(t)
	before := f.binding(t)
	if value := previewUsage(t, f); value.Auth != "" || value.Reason != routing.Warm {
		t.Fatal(value)
	}
	// No model request occurs: the next UI poll sees that cache protection ended.
	f.clock.Add(int64(6 * time.Minute))
	value := previewUsage(t, f)
	if value.Auth != "b" || value.Reason != routing.ColdMove || value.Mode != "active" || value.Forced || value.ExpiresMS <= value.AtMS {
		t.Fatal(value)
	}
	if after := f.binding(t); after != before {
		t.Fatal("preview changed affinity or consumed checkpoint", before, after)
	}
	if _, active, err := f.p.chats.Get(before.Key); err != nil || active {
		t.Fatal("preview created an active flight", err)
	}
	// Warmth is real evidence, not UI activity; a newly confirmed write hides the preference.
	r := f.before(t, "warm-again", "claude-opus-5-5")
	if got := f.after(t, r, "a"); got.Terminate {
		t.Fatal(string(got.ResponseBody))
	}
	if err := f.p.chats.CacheActivityForAuth(r.RequestID, "a", f.p.now(), true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	f.complete(t, r.RequestID)
	if value := previewUsage(t, f); value.Auth != "" || value.Reason != routing.Warm {
		t.Fatal(value)
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
