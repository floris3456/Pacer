package pacer

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUnboundAuxiliaryAcknowledgesOnlyItsRequestNotAPrimaryBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	defer func() { p.Close() }()
	q := tracker.Claim{Protocol: 1, Installation: "install", Session: "main-chat", Route: "separate-summary-route", Request: "helper-nonce", Kind: "compaction", Capability: tracker.Digest("test-private-capability"), Auto: true, CompactOnSwitch: true, Checkpoint: "unchanged"}
	key := tracker.Key("caller", "claude", q)
	admit := func(id, auth string) pluginapi.RequestInterceptResponse {
		t.Helper()
		raw, _ := json.Marshal(q)
		req := pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", Headers: http.Header{controlHeader: {base64.RawURLEncoding.EncodeToString(raw)}, "X-Quota-Auxiliary": {"spoofed-input"}}, Metadata: map[string]any{"caller_scope": "caller", "selected_auth_id": auth}}
		before := invoke(t, p, pluginabi.MethodRequestInterceptBefore, req).(pluginapi.RequestInterceptResponse)
		if before.Terminate {
			t.Fatal(string(before.ResponseBody))
		}
		if headers := p.ack(id); len(headers) != 0 {
			t.Fatal("before admission fabricated ack", headers)
		}
		return invoke(t, p, pluginabi.MethodRequestInterceptAfter, req).(pluginapi.RequestInterceptResponse)
	}
	for _, kind := range []string{"compaction", "title", "generate"} {
		q.Kind = kind
		q.Request = "nonce-" + kind
		after := admit(kind, "a")
		if after.Terminate {
			t.Fatal(string(after.ResponseBody))
		}
		cleared := false
		for _, name := range after.ClearHeaders {
			cleared = cleared || name == "X-Quota-Auxiliary"
		}
		if !cleared {
			t.Fatal("auxiliary coordination forwarded upstream")
		}
		for _, stream := range []bool{false, true} {
			var headers http.Header
			var clear []string
			if stream {
				r := invoke(t, p, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: kind, ChunkIndex: -1}).(pluginapi.StreamChunkInterceptResponse)
				headers, clear = r.Headers, r.ClearHeaders
			} else {
				r := invoke(t, p, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{RequestID: kind}).(pluginapi.ResponseInterceptResponse)
				headers, clear = r.Headers, r.ClearHeaders
			}
			if headers.Get("X-Quota-Protocol") != "1" || headers.Get("X-Quota-Binding") != "" || !reflect.DeepEqual(clear, quotaAckHeaders()) {
				t.Fatal("invalid auxiliary-only acknowledgement", headers, clear)
			}
			raw, err := base64.RawURLEncoding.DecodeString(headers.Get("X-Quota-Auxiliary"))
			var body map[string]any
			if err != nil || json.Unmarshal(raw, &body) != nil || !reflect.DeepEqual(body, map[string]any{"protocol": float64(1), "kind": kind, "request": q.Request}) {
				t.Fatal("ack not exactly request-bound", string(raw), err)
			}
		}
		invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: kind})
		b, active, err := p.chats.Get(key)
		if err != nil || b.Key != "" || active {
			t.Fatal("helper created a primary binding", b, active, err)
		}
	}
	p.Close()
	p = startPlugin(t, path)
	if b, _, err := p.chats.Get(key); err != nil || b.Key != "" {
		t.Fatal("restart invented auxiliary binding", err)
	}
	q.Kind = "primary"
	q.Request = "primary"
	if got := admit("primary", "a"); got.Terminate {
		t.Fatal(string(got.ResponseBody))
	}
	if h := p.ack("primary"); h.Get("X-Quota-Binding") == "" || h.Get("X-Quota-Auxiliary") != "" {
		t.Fatal("primary accepted only auxiliary ack", h)
	}
	invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "primary"})
	before, _, err := p.chats.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	q.Kind = "compaction"
	q.Request = "bound-helper"
	if got := admit("bound-helper", "b"); got.Terminate {
		t.Fatal(string(got.ResponseBody))
	}
	if h := p.ack("bound-helper"); h.Get("X-Quota-Binding") == "" || h.Get("X-Quota-Auxiliary") != "" {
		t.Fatal("existing-route ack changed", h)
	}
	after, _, err := p.chats.Get(key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("summary moved/consumed primary binding", before, after, err)
	}
	q.Capability = tracker.Digest("foreign")
	q.Request = "foreign"
	if got := admit("foreign", "b"); !got.Terminate {
		t.Fatal("foreign capability admitted")
	}
	if h := p.ack("foreign"); len(h) != 0 {
		t.Fatal("refused auxiliary acknowledged", h)
	}
}

func TestAuxiliaryAcknowledgementRequiresExplicitPacerDiscovery(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	r := p.events(pluginapi.ManagementRequest{Headers: http.Header{}})
	var body struct {
		Protocol int    `json:"protocol"`
		Pacer    bool   `json:"pacer"`
		Mode     string `json:"mode"`
		Aux      struct {
			Version int      `json:"version"`
			Kinds   []string `json:"kinds"`
		} `json:"auxiliary_ack"`
	}
	if json.Unmarshal(r.Body, &body) != nil || body.Protocol != 1 || !body.Pacer || body.Mode != "off" || body.Aux.Version != 1 || !reflect.DeepEqual(body.Aux.Kinds, []string{"compaction", "title", "generate"}) {
		t.Fatal(string(r.Body))
	}
}
