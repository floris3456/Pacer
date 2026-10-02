package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

func TestUsageResourceAndHostConfirmedCredential(t *testing.T) {
	p := New(nil)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"),
		Adaptive: adaptive.Config{Mode: "shadow"}, Credentials: []handoff.Credential{
			{ID: "claude-a", Index: "1", Provider: "claude", Account: "a", Alias: "Claude A"},
			{ID: "claude-b", Index: "2", Provider: "claude", Account: "b", Alias: "Claude B"},
			{ID: "codex-c", Index: "3", Provider: "codex", Account: "c", Alias: "Codex C"},
		}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Now().Unix()
	for _, c := range p.Engine.Credentials() {
		_ = p.Engine.Observe(c.ID, []quota.Window{{Scope: "all", Seconds: 18000, Remaining: 61, Observed: now, Reset: now + 3600}})
	}
	invoke := func(method string, request any) any {
		t.Helper()
		raw, _ := json.Marshal(request)
		result, err := p.Handle(method, raw)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	token := strings.Repeat("t", 48)
	claim := handoff.Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "proxy/sonnet", Request: "request", Kind: "primary", Capability: handoff.Digest(token), Auto: true, Fresh: true, Portable: true}
	encoded, _ := json.Marshal(claim)
	invoke(pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: "request", Model: "claude-sonnet", Body: []byte(`{"input":"dummy"}`), Headers: http.Header{ControlHeader: {base64.RawURLEncoding.EncodeToString(encoded)}}, Metadata: map[string]any{"caller_scope": "caller"}})
	invoke(pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "request", Model: "claude-sonnet", Metadata: map[string]any{"selected_auth_id": "claude-b"}})
	query := func(auth, view, all string) pluginapi.ManagementResponse {
		t.Helper()
		return invoke(pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/events", Headers: http.Header{"Authorization": {auth}}, Query: url.Values{"view": {view}, "all": {all}, "model": {claim.Route}}}).(pluginapi.ManagementResponse)
	}
	for _, auth := range []string{"", "Bearer " + strings.Repeat("x", 48)} {
		if response := query(auth, "usage", "true"); response.StatusCode != 401 {
			t.Fatal("usage must never share anonymous discovery behavior", response)
		}
	}
	for _, all := range []string{"false", "true"} {
		response := query("Bearer "+token, "usage", all)
		var report handoff.Usage
		if response.StatusCode != 200 || json.Unmarshal(response.Body, &report) != nil {
			t.Fatal(response)
		}
		if report.Current != "claude-b" || len(report.Active) != 1 || report.Active[0] != "claude-b" {
			t.Fatal("not the actual host-selected credential", report)
		}
		want := 2
		if all == "true" {
			want = 3
		}
		if len(report.Credentials) != want {
			t.Fatal("wrong credential scope", report)
		}
	}
	if response := query("Bearer "+token, "usage", "garbage"); response.StatusCode != 400 {
		t.Fatal(response)
	}
	if response := query("", "", ""); response.StatusCode != 200 {
		t.Fatal("existing protocol discovery changed", response)
	}
}
