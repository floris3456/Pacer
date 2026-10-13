package pacer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const claudeReplayPath = "/v0/resource/plugins/pacer/events"

func replaySupported(s pluginapi.ClaudeReplayCapability, kind string) bool {
	return s.Version == 1 && s.FinalWire && s.ExactCredential &&
		(kind == "keepalive" && s.RootKeepalive || kind == "native_compaction" && s.NativeCompaction)
}

func (p *Plugin) claudeReplaySupportedLocked(kind string) bool {
	return p.hostSchema >= 7 && p.hostGuard.enforced() && replaySupported(p.hostClaudeReplay, kind)
}

func (p *Plugin) registrationLocked() any {
	result := registration(p.policy != nil, p.forward != nil || p.cfg.QuotaWindowStart.Enabled).(map[string]any)
	s := p.hostCodexOrigin
	result["capabilities"].(map[string]bool)["codex_origin"] = s.Version == 1 && s.PhysicalHTTP && s.NativeCompact && p.codexOrigins != nil && p.hostGuard.accountPoolEnforced()
	return result
}

// Only this resource accepts a client command. The host authenticates its HTTP
// route and binds host_callback_id to the live caller before entering this RPC.
func claudeReplayRPC(raw []byte) bool {
	var req pluginapi.ManagementRequest
	return json.Unmarshal(raw, &req) == nil && req.Method == http.MethodPost && req.Path == claudeReplayPath
}

func replayHeader(headers http.Header, name string) string {
	values := headers.Values(name)
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 256 {
		return ""
	}
	for _, c := range values[0] {
		if c < 33 || c > 126 {
			return ""
		}
	}
	return values[0]
}

func (p *Plugin) claudeReplay(raw []byte) pluginapi.ManagementResponse {
	var envelope struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return clientError(400, "invalid_request", "correct_request")
	}
	req := envelope.ManagementRequest
	if req.Query.Get("view") != "claude-replay" || req.Query.Get("auth") != "api-key" || req.Query.Get("v") != "1" || len(req.Query) != 3 {
		return clientError(422, "unsupported_version", "upgrade")
	}
	for _, values := range req.Query {
		if len(values) != 1 {
			return clientError(400, "invalid_request", "correct_request")
		}
	}
	if len(req.Body) > (4<<20)+(16<<10) {
		return clientError(413, "request_too_large", "reduce_request")
	}
	command, err := decodeReplayCommand(req.Body)
	if err != nil ||
		(command.Operation != "acquire" && command.Operation != "execute") ||
		(command.Kind != "keepalive" && command.Kind != "native_compaction") ||
		(command.Operation == "acquire" && (command.Token != "" || len(command.Request) != 0)) ||
		(command.Operation == "execute" && (command.Token == "" || len(command.Token) > 256)) ||
		(command.Kind == "keepalive" && len(command.Request) != 0) ||
		(command.Operation == "execute" && command.Kind == "native_compaction" && (len(command.Request) == 0 || command.Request[0] != '{')) {
		return clientError(400, "invalid_request", "correct_request")
	}
	if len(command.Request) > 4<<20 {
		return clientError(413, "request_too_large", "reduce_request")
	}
	authorization := req.Headers.Values("Authorization")
	if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") || len(authorization[0]) <= 7 || len(authorization[0]) > 8199 || envelope.HostCallbackID == "" {
		return clientError(401, "unauthorized", "authenticate")
	}
	caller := clientCaller(strings.TrimPrefix(authorization[0], "Bearer "))
	p.mu.Lock()
	available := p.claudeReplaySupportedLocked(command.Kind) && p.policy != nil && !p.stopped
	allowed := p.clientAllowedLocked(caller, "")
	p.mu.Unlock()
	if !allowed {
		return clientError(403, "account_access_denied", "contact_operator")
	}
	if !available {
		return clientError(422, "host_integration_required", "upgrade")
	}
	header := func(name string) string { return replayHeader(req.Headers, "X-Pacer-Cache-"+name) }
	scope := pluginapi.CachePolicyScope{CallerScope: caller, InstallationID: header("Installation"), SessionID: header("Session"), ProviderRoute: header("Route")}
	revision, err := strconv.ParseInt(header("Revision"), 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != header("Revision") || header("Protocol") != "1" || scope.InstallationID == "" || scope.SessionID == "" || scope.ProviderRoute == "" || header("Epoch") == "" || header("Capability") == "" {
		return clientError(400, "invalid_policy_reference", "refresh_policy")
	}
	// No caller-selected account or URL enters the host. The callback verifies
	// this principal, policy and exact credential again immediately before IO.
	result, err := p.Host("host.claude_replay", pluginapi.HostClaudeReplayRequest{
		HostCallbackID: envelope.HostCallbackID, Operation: command.Operation, Kind: command.Kind,
		Scope: scope, ContextEpoch: header("Epoch"), Revision: revision, Capability: header("Capability"),
		Token: command.Token, Request: command.Request,
	})
	if err != nil {
		return clientError(409, "replay_unavailable", "refresh_policy")
	}
	var response pluginapi.HostClaudeReplayResponse
	if len(result) > 2<<20 || json.Unmarshal(result, &response) != nil || response.Version != 1 {
		return clientError(502, "invalid_host_response", "contact_operator")
	}
	return resource(200, response)
}

// JSON field names are protocol identifiers: duplicates and case aliases must
// not be resolved differently by the host and the client boundary.
type replayCommand struct {
	Operation string          `json:"operation"`
	Kind      string          `json:"kind"`
	Token     string          `json:"token,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
}

func decodeReplayCommand(body []byte) (replayCommand, error) {
	var command replayCommand
	d := json.NewDecoder(bytes.NewReader(body))
	invalid := errors.New("invalid replay command")
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return command, invalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return command, invalid
		}
		seen[name] = true
		switch name {
		case "operation":
			err = d.Decode(&command.Operation)
		case "kind":
			err = d.Decode(&command.Kind)
		case "token":
			err = d.Decode(&command.Token)
		case "request":
			err = d.Decode(&command.Request)
		default:
			return command, invalid
		}
		if err != nil {
			return command, invalid
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return command, invalid
	}
	if d.Decode(new(any)) != io.EOF {
		return command, invalid
	}
	if command.Operation == "acquire" && (seen["token"] || seen["request"]) {
		return command, invalid
	}
	command.Request = bytes.TrimSpace(command.Request)
	return command, nil
}
