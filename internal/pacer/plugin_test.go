package pacer

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func invoke(t *testing.T, p *Plugin, method string, request any) any {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func testHost(method string, input any) (json.RawMessage, error) {
	var data any
	switch method {
	case "host.auth.list":
		data = map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "ia", "provider": "claude", "label": "A"}, {"id": "b", "auth_index": "ib", "provider": "claude", "label": "B"}}}
	case "host.auth.get":
		index := input.(map[string]string)["auth_index"]
		data = map[string]any{"json": map[string]string{"account_uuid": index}}
	case "host.affinity.lookup":
		data = pluginapi.HostAffinityLookupResponse{Status: "unbound"}
	default:
		data = map[string]any{}
	}
	return json.Marshal(data)
}

func startPlugin(t *testing.T, path string) *Plugin {
	t.Helper()
	p := New(testHost)
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\npriority: 1\nstate_path: " + path + "\ncompact_on_switch: true\nrouting:\n  mode: off\n")})
	return p
}

func TestRegistrationSatisfiesCPAMetadataAndCapabilities(t *testing.T) {
	// The native loader requires all four metadata fields. A successful C ABI
	// call alone does not establish that CPA registered the plugin.
	raw, err := json.Marshal(registration())
	if err != nil {
		t.Fatal(err)
	}
	var registered struct {
		Schema       int                `json:"schema_version"`
		Metadata     pluginapi.Metadata `json:"metadata"`
		Capabilities map[string]bool    `json:"capabilities"`
	}
	if err = json.Unmarshal(raw, &registered); err != nil {
		t.Fatal(err)
	}
	m := registered.Metadata
	if registered.Schema != 6 || m.Name != "pacer" || m.Version == "" || m.Author == "" || m.GitHubRepository == "" {
		t.Fatalf("incomplete CPA registration: %+v", registered)
	}
	for _, key := range []string{"scheduler", "request_interceptor", "request_lifecycle_plugin", "response_interceptor", "response_stream_interceptor", "usage_plugin", "management_api"} {
		if !registered.Capabilities[key] {
			t.Fatal("missing capability", key)
		}
	}
}

func TestNativeDispatchForcedMoveStopsBeforeFakeUpstreamAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pacer.sqlite")
	p := startPlugin(t, path)
	defer func() { p.Close() }()
	var mu sync.Mutex
	bodies := map[string][]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.Header.Get("Test-Account")] = append(bodies[r.Header.Get("Test-Account")], string(body))
		mu.Unlock()
		if r.Header.Get("Test-Exhausted") == "yes" {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()
	q := tracker.Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "claude/opus", Request: "logical", Kind: "primary", Capability: tracker.Digest("private-test-capability"), Auto: true, CompactOnSwitch: true, Checkpoint: "old"}
	run := func(id, auth, body string, exhausted bool) int {
		t.Helper()
		claimJSON, _ := json.Marshal(q)
		headers := http.Header{controlHeader: {base64.RawURLEncoding.EncodeToString(claimJSON)}, "X-Session-Affinity": {"chat"}}
		req := pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", Headers: headers, Metadata: map[string]any{"caller_scope": "test-caller", "selected_auth_id": auth}}
		before := invoke(t, p, pluginabi.MethodRequestInterceptBefore, req).(pluginapi.RequestInterceptResponse)
		if before.Terminate {
			return before.StatusCode
		}
		for k, v := range before.Headers {
			headers[k] = v
		}
		decision := invoke(t, p, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{Provider: "claude", Model: req.Model, Options: pluginapi.SchedulerOptions{Headers: headers, Metadata: req.Metadata}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: auth, Provider: "claude"}}}).(pluginapi.SchedulerPickResponse)
		if decision.Handled {
			t.Fatal("routing-off changed CPA's choice")
		}
		after := invoke(t, p, pluginabi.MethodRequestInterceptAfter, req).(pluginapi.RequestInterceptResponse)
		defer invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: id})
		if after.Terminate {
			if !strings.Contains(string(after.ResponseBody), "quota_compact_required") {
				t.Fatal(string(after.ResponseBody))
			}
			return after.StatusCode
		}
		for _, k := range after.ClearHeaders {
			delete(headers, k)
		}
		request, _ := http.NewRequest("POST", upstream.URL, strings.NewReader(body))
		request.Header = headers
		request.Header.Set("Test-Account", auth)
		if exhausted {
			request.Header.Set("Test-Exhausted", "yes")
		}
		response, err := upstream.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	if code := run("initial", "a", "full history", false); code != 200 {
		t.Fatal(code)
	}
	if code := run("exhausted", "a", "full history", true); code != 429 {
		t.Fatal(code)
	}
	p.Close()
	p = startPlugin(t, path)
	if code := run("refused", "b", "full history", false); code != 409 {
		t.Fatal(code)
	}
	mu.Lock()
	count := len(bodies["b"])
	mu.Unlock()
	if count != 0 {
		t.Fatal("new upstream received uncompacted context")
	}
	q.Kind = "compaction"
	if code := run("summary", "b", "summary input", false); code != 200 {
		t.Fatal(code)
	}
	q.Kind = "primary"
	if code := run("still-old", "b", "full history", false); code != 409 {
		t.Fatal("summary attempt falsely counted as committed checkpoint", code)
	}
	q.Checkpoint = "committed-new"
	if code := run("resumed", "b", "compacted checkpoint", false); code != 200 {
		t.Fatal(code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies["b"]) != 2 || bodies["b"][1] != "compacted checkpoint" {
		t.Fatal(bodies["b"])
	}
}

func TestPublicResourceCannotReadManagementStateOrChangeRouting(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "pacer.sqlite"))
	defer p.Close()
	for _, req := range []pluginapi.ManagementRequest{{Method: "GET", Path: "/v0/resource/plugins/pacer/state"}, {Method: "PATCH", Path: "/v0/resource/plugins/pacer/routing", Body: []byte(`{"mode":"on"}`)}, {Method: "GET", Path: "/v0/resource/plugins/pacer/events", Headers: http.Header{"Authorization": {"Bearer guessed"}}}} {
		response := invoke(t, p, pluginabi.MethodManagementHandle, req).(pluginapi.ManagementResponse)
		if response.StatusCode == 200 {
			t.Fatalf("public request reached private data: %s", req.Path)
		}
	}
	response := invoke(t, p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"active"}`)}).(pluginapi.ManagementResponse)
	if response.StatusCode != 400 || p.cfg.Routing.Mode != "off" {
		t.Fatal("invalid mode accepted")
	}
}
