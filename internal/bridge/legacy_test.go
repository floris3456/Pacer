package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

func TestLegacyWarningsAreSuppressedForCoordinatedRequests(t *testing.T) {
	p := New(nil)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), Credentials: []handoff.Credential{{ID: "small", Index: "1", Provider: "claude", Account: "small", Alias: "Small", Rank: 1}}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Now().Unix()
	if err := p.Engine.Observe("small", []quota.Window{{Scope: "all", Seconds: 18000, Remaining: 20, Reset: now + 18000, Observed: now}, {Scope: "all", Seconds: 604800, Remaining: 100, Reset: now + 604800, Observed: now}}); err != nil {
		t.Fatal(err)
	}
	invoke := func(method string, input any) any {
		raw, _ := json.Marshal(input)
		result, err := p.Handle(method, raw)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, coordinated := range []bool{false, true} {
		id := "legacy"
		headers := http.Header{}
		if coordinated {
			id = "coordinated"
			q := handoff.Claim{Protocol: 1, Installation: "test", Session: "a", Request: "a", Route: "claude", Kind: "primary", Capability: handoff.Digest("test"), Auto: true, Fresh: true, Portable: true}
			raw, _ := json.Marshal(q)
			headers.Set(ControlHeader, base64.RawURLEncoding.EncodeToString(raw))
		}
		before := invoke(pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: id, Headers: headers, Metadata: map[string]any{"caller_scope": "test"}}).(pluginapi.RequestInterceptResponse)
		if before.Terminate {
			t.Fatal(before)
		}
		if !coordinated {
			invoke(pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: id, Metadata: map[string]any{"selected_auth_id": "small"}})
		}
		req := pluginapi.StreamChunkInterceptRequest{RequestID: id, SourceFormat: "openai", Model: "claude-sonnet", OriginalRequest: []byte(`{"stream":true}`), ChunkIndex: -1}
		invoke(pluginabi.MethodResponseInterceptStreamChunk, req)
		req.ChunkIndex = 0
		req.Metadata = nil // CPA can omit selected-auth identity on every response callback.
		req.Body = []byte(`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`)
		result := invoke(pluginabi.MethodResponseInterceptStreamChunk, req).(pluginapi.StreamChunkInterceptResponse)
		if result.DropChunk || (strings.Contains(string(result.Body), "CPA quota warning") == coordinated) {
			t.Fatalf("coordinated=%v result=%+v", coordinated, result)
		}
		invoke(pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: id})
		if len(p.requests) != 0 {
			t.Fatal("request state leaked")
		}
	}
}
