package pacer

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type replayEnvelope struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id"`
}

func replayInput() replayEnvelope {
	return replayEnvelope{ManagementRequest: pluginapi.ManagementRequest{
		Method: http.MethodPost, Path: claudeReplayPath,
		Query: url.Values{"view": {"claude-replay"}, "auth": {"api-key"}, "v": {"1"}},
		Headers: http.Header{"Authorization": {"Bearer replay-caller"},
			"X-Pacer-Cache-Protocol": {"1"}, "X-Pacer-Cache-Installation": {"installation"},
			"X-Pacer-Cache-Session": {"session"}, "X-Pacer-Cache-Route": {"a-main"},
			"X-Pacer-Cache-Epoch": {"checkpoint"}, "X-Pacer-Cache-Revision": {"2"},
			"X-Pacer-Cache-Capability": {strings.Repeat("a", 42) + "A"}},
		Body: []byte(`{"operation":"acquire","kind":"native_compaction"}`),
	}, HostCallbackID: "live-authenticated-callback"}
}

func replayPlugin(t *testing.T) (*Plugin, *[]pluginapi.HostClaudeReplayRequest) {
	t.Helper()
	p := auxiliaryRegisteredPlugin(t, hostCachePolicySupport{3, true}, 7, true)
	p.hostClaudeReplay = pluginapi.ClaudeReplayCapability{Version: 1, FinalWire: true, ExactCredential: true, RootKeepalive: true, NativeCompaction: true}
	var calls []pluginapi.HostClaudeReplayRequest
	p.Host = func(method string, input any) (json.RawMessage, error) {
		if method != "host.claude_replay" {
			return testHost(method, input)
		}
		request, ok := input.(pluginapi.HostClaudeReplayRequest)
		if !ok {
			t.Fatalf("unexpected callback type %T", input)
		}
		calls = append(calls, request)
		return json.RawMessage(`{"version":1,"state":"unavailable"}`), nil
	}
	return p, &calls
}

func replayResponse(t *testing.T, p *Plugin, input replayEnvelope) pluginapi.ManagementResponse {
	t.Helper()
	return invoke(t, p, pluginabi.MethodManagementHandle, input).(pluginapi.ManagementResponse)
}

func TestClaudeReplayBindsCallerPolicyAndNeverForwardsAccountChoice(t *testing.T) {
	p, calls := replayPlugin(t)
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: clientCaller("replay-caller"), AuthIDs: []string{"private-account"}}}}
	input := replayInput()
	input.Headers.Set("X-Pacer-Caller-Scope", clientCaller("other-caller"))
	input.Headers.Set("X-Pacer-Selected-Auth-ID", "chosen-account")
	input.Headers.Set("X-Pacer-Upstream-URL", "https://untrusted.invalid")
	response := replayResponse(t, p, input)
	if response.StatusCode != 200 || len(*calls) != 1 {
		t.Fatalf("valid scoped replay: %d %s calls=%d", response.StatusCode, response.Body, len(*calls))
	}
	got := (*calls)[0]
	if got.Scope.CallerScope != clientCaller("replay-caller") || got.Scope.InstallationID != "installation" || got.Scope.SessionID != "session" || got.Scope.ProviderRoute != "a-main" || got.ContextEpoch != "checkpoint" || got.Revision != 2 || got.Capability != input.Headers.Get("X-Pacer-Cache-Capability") || got.HostCallbackID != input.HostCallbackID {
		t.Fatalf("live caller/policy binding changed: %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"replay-caller", "chosen-account", "private-account", "untrusted.invalid", clientCaller("other-caller")} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("caller-selected or private identity crossed callback: %s", forbidden)
		}
	}
}

func TestClaudeReplayRejectsMalformedOrDeniedWithoutHostCallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(*Plugin, *replayEnvelope)
		status int
	}{
		{"missing bearer", func(_ *Plugin, r *replayEnvelope) { r.Headers.Del("Authorization") }, 401},
		{"duplicate bearer", func(_ *Plugin, r *replayEnvelope) { r.Headers.Add("Authorization", "Bearer second") }, 401},
		{"missing live callback", func(_ *Plugin, r *replayEnvelope) { r.HostCallbackID = "" }, 401},
		{"denied caller", func(p *Plugin, _ *replayEnvelope) { p.cfg.ClientAccess = &ClientAccess{} }, 403},
		{"foreign grant", func(p *Plugin, _ *replayEnvelope) {
			p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: clientCaller("different"), AuthIDs: []string{"a"}}}}
		}, 403},
		{"extra query", func(_ *Plugin, r *replayEnvelope) { r.Query.Set("account", "chosen") }, 422},
		{"duplicate query", func(_ *Plugin, r *replayEnvelope) { r.Query.Add("view", "claude-replay") }, 400},
		{"wrong version", func(_ *Plugin, r *replayEnvelope) { r.Query.Set("v", "2") }, 422},
		{"malformed JSON", func(_ *Plugin, r *replayEnvelope) { r.Body = []byte(`{`) }, 400},
		{"trailing JSON", func(_ *Plugin, r *replayEnvelope) { r.Body = append(r.Body, []byte(` {}`)...) }, 400},
		{"chosen account", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"acquire","kind":"native_compaction","auth_id":"chosen"}`)
		}, 400},
		{"chosen URL", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"acquire","kind":"native_compaction","url":"https://other.invalid"}`)
		}, 400},
		{"unknown kind", func(_ *Plugin, r *replayEnvelope) { r.Body = []byte(`{"operation":"acquire","kind":"primary"}`) }, 400},
		{"acquire with token", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"acquire","kind":"native_compaction","token":"chosen"}`)
		}, 400},
		{"acquire with request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"acquire","kind":"native_compaction","request":{}}`)
		}, 400},
		{"acquire null request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"acquire","kind":"native_compaction","request":null}`)
		}, 400},
		{"acquire null token", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"acquire","kind":"native_compaction","token":null}`)
		}, 400},
		{"execute without token", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"native_compaction","request":{}}`)
		}, 400},
		{"native execute without request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"native_compaction","token":"lease"}`)
		}, 400},
		{"native null request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"native_compaction","token":"lease","request":null}`)
		}, 400},
		{"native scalar request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"native_compaction","token":"lease","request":1}`)
		}, 400},
		{"native array request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"native_compaction","token":"lease","request":[]}`)
		}, 400},
		{"oversize token", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"keepalive","token":"` + strings.Repeat("x", 257) + `"}`)
		}, 400},
		{"keepalive with body", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"keepalive","token":"lease","request":{}}`)
		}, 400},
		{"duplicate operation", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","operation":"acquire","kind":"native_compaction"}`)
		}, 400},
		{"case alias", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"Operation":"acquire","kind":"native_compaction"}`)
		}, 400},
		{"missing scope", func(_ *Plugin, r *replayEnvelope) { r.Headers.Del("X-Pacer-Cache-Session") }, 400},
		{"duplicate scope", func(_ *Plugin, r *replayEnvelope) { r.Headers.Add("X-Pacer-Cache-Session", "another") }, 400},
		{"space in scope", func(_ *Plugin, r *replayEnvelope) { r.Headers.Set("X-Pacer-Cache-Session", "not an identifier") }, 400},
		{"oversize scope", func(_ *Plugin, r *replayEnvelope) { r.Headers.Set("X-Pacer-Cache-Session", strings.Repeat("x", 257)) }, 400},
		{"zero revision", func(_ *Plugin, r *replayEnvelope) { r.Headers.Set("X-Pacer-Cache-Revision", "0") }, 400},
		{"noncanonical revision", func(_ *Plugin, r *replayEnvelope) { r.Headers.Set("X-Pacer-Cache-Revision", "02") }, 400},
		{"unsafe revision", func(_ *Plugin, r *replayEnvelope) { r.Headers.Set("X-Pacer-Cache-Revision", "9223372036854775808") }, 400},
		{"wrong protocol", func(_ *Plugin, r *replayEnvelope) { r.Headers.Set("X-Pacer-Cache-Protocol", "2") }, 400},
		{"oversize command", func(_ *Plugin, r *replayEnvelope) { r.Body = []byte(strings.Repeat(" ", (4<<20)+(16<<10)+1)) }, 413},
		{"oversize nested request", func(_ *Plugin, r *replayEnvelope) {
			r.Body = []byte(`{"operation":"execute","kind":"native_compaction","token":"lease","request":{"input":"` + strings.Repeat("x", 4<<20) + `"}}`)
		}, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, calls := replayPlugin(t)
			input := replayInput()
			tc.edit(p, &input)
			response := replayResponse(t, p, input)
			if response.StatusCode != tc.status || len(*calls) != 0 {
				t.Fatalf("malformed/denied request reached host: status=%d want=%d calls=%d body=%s", response.StatusCode, tc.status, len(*calls), response.Body)
			}
		})
	}
}

func TestClaudeReplayAdvertisementAndDispatchRequireSameHostGuarantees(t *testing.T) {
	for _, tc := range []struct {
		name      string
		edit      func(*Plugin)
		supported bool
		enabled   bool
	}{
		{"attested host", func(*Plugin) {}, true, true},
		{"old schema", func(p *Plugin) { p.hostSchema = 6 }, false, false},
		{"unknown revision", func(p *Plugin) { p.hostClaudeReplay.Version = 2 }, false, false},
		{"no final wire", func(p *Plugin) { p.hostClaudeReplay.FinalWire = false }, false, false},
		{"no exact credential", func(p *Plugin) { p.hostClaudeReplay.ExactCredential = false }, false, false},
		{"no native support", func(p *Plugin) { p.hostClaudeReplay.NativeCompaction = false }, false, false},
		{"missing required guard", func(p *Plugin) { p.hostGuard.Required = false }, false, false},
		{"no configured policy", func(p *Plugin) { p.policy = nil }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, calls := replayPlugin(t)
			tc.edit(p)
			feature := p.clientAPILocked().Features["native_compaction"]
			if feature.Supported != tc.supported || feature.Enabled != tc.enabled || feature.ContractRevision != 1 {
				t.Fatalf("incorrect native advertisement: %+v", feature)
			}
			response := replayResponse(t, p, replayInput())
			if tc.enabled {
				if response.StatusCode != 200 || len(*calls) != 1 {
					t.Fatal(response.StatusCode, len(*calls))
				}
			} else if response.StatusCode != 422 || len(*calls) != 0 {
				t.Fatal("unsupported host received command", response.StatusCode, len(*calls))
			}
		})
	}
}

func TestClaudeReplayHostFailureIsBoundedAndNeverExposesInternalError(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		err  error
		want int
	}{
		{"callback error", nil, errors.New("private credential details"), 409},
		{"malformed response", json.RawMessage(`{`), nil, 502},
		{"unknown version", json.RawMessage(`{"version":2}`), nil, 502},
		{"oversize response", json.RawMessage(strings.Repeat(" ", (2<<20)+1)), nil, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := replayPlugin(t)
			p.Host = func(string, any) (json.RawMessage, error) { return tc.raw, tc.err }
			response := replayResponse(t, p, replayInput())
			if response.StatusCode != tc.want || strings.Contains(string(response.Body), "private credential") {
				t.Fatal(response.StatusCode, string(response.Body))
			}
		})
	}
}
