package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func switchPlugin(t *testing.T) (*Plugin, func(gate string)) {
	t.Helper()
	p := New(func(string, any) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	state := filepath.Join(t.TempDir(), "state.sqlite")
	configure := func(gate string) {
		t.Helper()
		cfg := fmt.Sprintf("%sstate_path: %s\nadaptive:\n  mode: shadow\ncredentials:\n"+
			"  - {auth_id: claude-a, auth_index: a, account: a, alias: Claude A, provider: claude}\n"+
			"  - {auth_id: claude-b, auth_index: b, account: b, alias: Claude B, provider: claude}\n", gate, state)
		method := pluginabi.MethodPluginRegister
		if p.Engine != nil {
			method = pluginabi.MethodPluginReconfigure
		}
		noticeInvoke(t, p, method, map[string]any{"config_yaml": []byte(cfg)})
	}
	configure("compact_on_switch: true\n")
	t.Cleanup(p.Close)
	return p, configure
}

func switchHeader(session, request, checkpoint string, declared bool) http.Header {
	claim := handoff.Claim{Route: "cliproxyapi/claude-opus-5-5", Protocol: 1, Installation: "install", Session: session, Request: request,
		Kind: "primary", Capability: handoff.Digest("capability-" + session), Auto: true, Portable: true, Checkpoint: checkpoint,
		Generation: 1, CompactOnSwitch: declared}
	raw, _ := json.Marshal(claim)
	return http.Header{ControlHeader: {base64.RawURLEncoding.EncodeToString(raw)}}
}

// attempt mirrors CPA: request.intercept_before once, then request.intercept_after
// for every credential the conductor tries, in order.
func attempt(t *testing.T, p *Plugin, id string, header http.Header, auths ...string) pluginapi.RequestInterceptResponse {
	t.Helper()
	meta := map[string]any{"caller_scope": "api-key-1"}
	noticeInvoke(t, p, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", Headers: header, Body: []byte(`{"input":"x"}`), Metadata: meta})
	var last pluginapi.RequestInterceptResponse
	for _, auth := range auths {
		after := map[string]any{"caller_scope": "api-key-1", "selected_auth_id": auth}
		last = noticeInvoke(t, p, pluginabi.MethodRequestInterceptAfter, pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", Headers: header, Metadata: after}).(pluginapi.RequestInterceptResponse)
		if last.Terminate {
			break
		}
	}
	return last
}

func TestCompactOnSwitchTerminatesBeforeTheNewAccountAndTogglesLive(t *testing.T) {
	p, configure := switchPlugin(t)
	if r := attempt(t, p, "r1", switchHeader("chat", "r1", "", true), "claude-a"); r.Terminate {
		t.Fatalf("first request refused: %s", r.ResponseBody)
	}
	noticeInvoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "r1", Outcome: pluginapi.RequestCompletionSucceeded})
	// A answers 429 and CPA's credential loop tries B for the same request.
	r := attempt(t, p, "r2", switchHeader("chat", "r2", "", true), "claude-a", "claude-b")
	var body struct {
		Error struct{ Code, Message, Type string } `json:"error"`
	}
	if !r.Terminate || r.StatusCode != 409 || r.ResponseHeaders.Get("X-Quota-Protocol") != "1" || json.Unmarshal(r.ResponseBody, &body) != nil ||
		body.Error.Code != "quota_compact_required" || body.Error.Message != "CPA moves this chat from Claude A to Claude B." {
		t.Fatalf("switch not refused before B: %+v %s", r, r.ResponseBody)
	}
	noticeInvoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "r2", Outcome: pluginapi.RequestCompletionFailed})
	// The retry after the client's compaction carries the new checkpoint and is admitted on B.
	if r := attempt(t, p, "r3", switchHeader("chat", "r3", "cmp_new", true), "claude-b"); r.Terminate {
		t.Fatalf("compacted chat refused: %s", r.ResponseBody)
	}
	ack := noticeInvoke(t, p, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: "r3", Metadata: map[string]any{"selected_auth_id": "claude-b"}}).(pluginapi.ResponseInterceptResponse)
	raw, _ := base64.RawURLEncoding.DecodeString(ack.Headers.Get(AckHeader))
	var binding struct {
		Generation int    `json:"generation"`
		Alias      string `json:"alias"`
	}
	if json.Unmarshal(raw, &binding) != nil || binding.Generation != 2 || binding.Alias != "Claude B" {
		t.Fatalf("binding acknowledgement after the switch: %s", raw)
	}
	noticeInvoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "r3", Outcome: pluginapi.RequestCompletionSucceeded})
	// Older clients (no declaration) are switched as before, even with the gate on.
	attempt(t, p, "o1", switchHeader("old", "o1", "", false), "claude-a")
	if r := attempt(t, p, "o2", switchHeader("old", "o2", "", false), "claude-b"); r.Terminate {
		t.Fatalf("undeclared client refused: %s", r.ResponseBody)
	}
	// compact_on_switch changes live (no reload error) and off restores the old behavior.
	attempt(t, p, "l1", switchHeader("live", "l1", "", true), "claude-a")
	configure("compact_on_switch: false\n")
	if r := attempt(t, p, "l2", switchHeader("live", "l2", "", true), "claude-b"); r.Terminate {
		t.Fatalf("gate off still refused: %s", r.ResponseBody)
	}
	configure("compact_on_switch: true\n")
	if r := attempt(t, p, "l3", switchHeader("live", "l3", "", true), "claude-a"); !r.Terminate {
		t.Fatal("gate on again did not refuse the next move")
	}
}
