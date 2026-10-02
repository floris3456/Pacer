package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type noticeTestLog struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields"`
}

func mutedPlugin(t *testing.T) (*Plugin, func(bool), func() []noticeTestLog) {
	t.Helper()
	var mu sync.Mutex
	var logs []noticeTestLog
	p := New(func(method string, input any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostLog {
			raw, _ := json.Marshal(input)
			var record noticeTestLog
			if err := json.Unmarshal(raw, &record); err != nil {
				return nil, err
			}
			mu.Lock()
			logs = append(logs, record)
			mu.Unlock()
		}
		return json.RawMessage(`{}`), nil
	})
	statePath := filepath.Join(t.TempDir(), "state.sqlite")
	configure := func(enabled bool) {
		t.Helper()
		cfg := fmt.Sprintf("notifications_enabled: %t\nstate_path: %s\nadaptive:\n  mode: shadow\ncredentials:\n  - auth_id: small\n    auth_index: '1'\n    account: small\n    alias: Small\n    provider: claude\n", enabled, statePath)
		method := pluginabi.MethodPluginRegister
		if p.Engine != nil {
			method = pluginabi.MethodPluginReconfigure
		}
		noticeInvoke(t, p, method, map[string]any{"config_yaml": []byte(cfg)})
	}
	configure(false)
	t.Cleanup(p.Close)
	return p, configure, func() []noticeTestLog {
		mu.Lock()
		defer mu.Unlock()
		return append([]noticeTestLog(nil), logs...)
	}
}

func noticeInvoke(t *testing.T, p *Plugin, method string, input any) any {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func noticeObserve(t *testing.T, p *Plugin, remaining float64) {
	t.Helper()
	now := time.Now().Unix()
	if err := p.Engine.Observe("small", []quota.Window{
		{Scope: "all", Seconds: 18000, Remaining: remaining, Reset: now + 18000, Observed: now},
		{Scope: "all", Seconds: 604800, Remaining: 100, Reset: now + 604800, Observed: now},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMutedStreamsKeepTextAndLogBothThresholds(t *testing.T) {
	p, _, logs := mutedPlugin(t)
	formats := []struct{ name, body string }{
		{"openai", `{"choices":[{"index":0,"delta":{"content":"answer"}}]}`},
		{"claude", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n"},
		{"openai-response", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"answer\"}\n\n"},
	}
	for _, remaining := range []float64{20, 10} {
		noticeObserve(t, p, remaining)
		for _, format := range formats {
			id := fmt.Sprintf("%s-%g", format.name, remaining)
			noticeInvoke(t, p, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: id, Model: "light", Body: []byte(`{"messages":[]}`)})
			after := noticeInvoke(t, p, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: id, Model: "light", Metadata: map[string]any{"selected_auth_id": "small"}}).(pluginapi.RequestInterceptResponse)
			if after.Terminate {
				t.Fatal("mute refused model request")
			}
			req := pluginapi.StreamChunkInterceptRequest{RequestID: id, Model: "light", SourceFormat: format.name, OriginalRequest: []byte(`{"stream":true}`), ChunkIndex: -1}
			noticeInvoke(t, p, pluginabi.MethodResponseInterceptStreamChunk, req)
			req.Body = []byte(format.body)
			for i := 0; i < 3; i++ {
				req.ChunkIndex = i
				res := noticeInvoke(t, p, pluginabi.MethodResponseInterceptStreamChunk, req).(pluginapi.StreamChunkInterceptResponse)
				if res.DropChunk || (len(res.Body) > 0 && string(res.Body) != format.body) {
					t.Fatalf("muted stream changed: %+v", res)
				}
			}
			noticeInvoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: id, Outcome: pluginapi.RequestCompletionSucceeded})
		}
	}
	var kinds []string
	for _, record := range logs() {
		if record.Level != "warning" {
			continue
		}
		if record.Fields["notifications_enabled"] != false || !strings.Contains(record.Message, "quota") {
			t.Fatalf("incomplete audit record: %+v", record)
		}
		kinds = append(kinds, record.Fields["quota_kind"].(string))
	}
	if fmt.Sprint(kinds) != "[warning reminder]" {
		t.Fatalf("expected one log per threshold despite repeated chunks/requests: %v", kinds)
	}
	if len(p.requests) != 0 {
		t.Fatal("completed requests retained")
	}
	raw, err := p.Engine.MarshalState()
	if err != nil || !strings.Contains(string(raw), `"remaining":10`) {
		t.Fatalf("quota state not retained: %s %v", raw, err)
	}
}

func TestMutedPollingRetainsDurableEventsAndReconfiguresLive(t *testing.T) {
	p, configure, logs := mutedPlugin(t)
	noticeObserve(t, p, 20)
	token := strings.Repeat("c", 48)
	claim := handoff.Claim{Protocol: 1, Installation: "test", Session: "chat", Request: "request", Route: "light", Kind: "primary", Capability: handoff.Digest(token), Auto: true, Fresh: true, Portable: true}
	raw, _ := json.Marshal(claim)
	headers := http.Header{ControlHeader: {base64.RawURLEncoding.EncodeToString(raw)}}
	noticeInvoke(t, p, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: "request", Model: "light", Headers: headers, Body: []byte(`{"messages":[]}`), Metadata: map[string]any{"caller_scope": "test"}})
	noticeInvoke(t, p, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: "request", Model: "light", Metadata: map[string]any{"selected_auth_id": "small"}})
	poll := func(after string) handoff.Snapshot {
		t.Helper()
		res := noticeInvoke(t, p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/events", Headers: http.Header{"Authorization": {"Bearer " + token}}, Query: url.Values{"model": {"light"}, "after": {after}}}).(pluginapi.ManagementResponse)
		if res.StatusCode != 200 {
			t.Fatalf("poll failed: %+v", res)
		}
		var snapshot handoff.Snapshot
		if err := json.Unmarshal(res.Body, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	muted := poll("0")
	if len(muted.Events) != 0 || muted.Cursor == 0 || len(muted.Windows) != 2 || muted.Mode != "shadow" || muted.Intent != "" {
		t.Fatalf("muted snapshot lost state or leaked events: %+v", muted)
	}
	stored, err := p.Engine.MarshalState()
	var state handoff.State
	if err != nil || json.Unmarshal(stored, &state) != nil {
		t.Fatal("cannot read durable state")
	}
	for _, binding := range state.Bindings {
		if len(binding.Events) == 0 {
			t.Fatal("muting erased durable notices")
		}
	}
	engine := p.Engine
	configure(true)
	if p.Engine != engine || len(p.requests) != 1 {
		t.Fatal("delivery toggle reset engine or interrupted request")
	}
	if len(poll("0").Events) == 0 {
		t.Fatal("explicit re-enable did not restore delivery")
	}
	if len(poll(fmt.Sprint(muted.Cursor)).Events) != 0 {
		t.Fatal("previously consumed notices were replayed")
	}
	configure(false)
	noticeObserve(t, p, 10)
	urgent := poll(fmt.Sprint(muted.Cursor))
	if len(urgent.Events) != 0 || urgent.Cursor <= muted.Cursor {
		t.Fatal("urgent event not retained and suppressed")
	}
	warnings := 0
	for _, record := range logs() {
		if record.Level == "warning" {
			warnings++
		}
	}
	if warnings < 2 {
		t.Fatal("suppressed events were not logged")
	}
	res := noticeInvoke(t, p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/events", Headers: http.Header{"Authorization": {"Bearer " + strings.Repeat("x", 48)}}, Query: url.Values{"model": {"light"}}}).(pluginapi.ManagementResponse)
	if res.StatusCode != 401 {
		t.Fatal("mute bypassed event authentication")
	}
}
