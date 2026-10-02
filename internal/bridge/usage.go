package bridge

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"strings"
)

// Reuse the exact GET route already allowed by Caddy. Session reads authenticate
// with their capability; pre-enrollment reads validate the ordinary CPA API key.
func (p *Plugin) usage(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	authorization := req.Headers.Get("Authorization")
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == authorization || len(token) == 0 || len(token) > 8192 {
		return resource(401, map[string]string{"error": "unauthorized"})
	}
	all := req.Query.Get("all")
	if all != "" && all != "true" && all != "false" {
		return resource(400, map[string]string{"error": "invalid_usage_scope"})
	}
	var result handoff.Usage
	var err error
	switch req.Query.Get("auth") {
	case "api-key":
		if all != "true" {
			return resource(400, map[string]string{"error": "usage_requires_all"})
		}
		accepted, unavailable := p.authenticateUsageKey(token)
		if unavailable != nil {
			return resource(503, map[string]string{"error": "usage_auth_unavailable"})
		}
		if !accepted {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		result, err = p.Engine.AllUsage()
		if err != nil {
			return resource(503, map[string]string{"error": "quota_state_unavailable"})
		}
	case "":
		if len(token) < 40 || len(token) > 128 {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		result, err = p.Engine.UsageView(token, req.Query.Get("model"), all == "true")
		if err != nil {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
	default:
		return resource(400, map[string]string{"error": "invalid_usage_auth"})
	}
	// Invalid credentials never trigger quota sampling. Reading usage does not
	// touch admission, chat liveness, acknowledgement or credential selection.
	go p.refresh(nil)
	return resource(200, result)
}
