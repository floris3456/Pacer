package pacer

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Admission uses host model names, including the selected upstream name after
// alias resolution. Thinking suffixes must not bypass an exact model policy.
// An alias may still be unsupported by managed enrollment; that fails closed.
func policyModel(model string) string {
	base, _, _ := strings.Cut(strings.TrimSpace(model), "(")
	return strings.TrimSpace(base)
}

// Caller holds p.mu. The selection is produced by the authenticated policy RPC,
// never by a declaration header. Check it before admission and on every retry.
func (p *Plugin) requiredPolicyLocked(req pluginapi.RequestInterceptRequest, caller string) pluginapi.RequestInterceptResponse {
	if !p.cfg.CachePolicy.RequireManaged {
		return pluginapi.RequestInterceptResponse{}
	}
	selected, exists := p.selections[req.RequestID]
	required, matchingRoute := false, false
	for _, route := range p.cfg.CachePolicy.Routes {
		for _, model := range route.Models {
			if model != policyModel(req.Model) && model != policyModel(req.RequestedModel) {
				continue
			}
			required = true
			if selected.Scope.ProviderRoute == route.ID {
				matchingRoute = true
			}
		}
	}
	if !required || exists && matchingRoute && caller != "" && selected.Scope.CallerScope == caller {
		return pluginapi.RequestInterceptResponse{}
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": "pacer_cache_policy_required", "type": "pacer_policy",
		"message": "This model requires an admitted cache policy. Configure a capable client and enroll its session before sending the request.",
		"action":  "enroll_cache_policy", "retryable": false,
	}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusConflict, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: body}
}

// Public requirements contain model IDs only, never callers or credential IDs.
func (p *Plugin) requiredModelsLocked() []string {
	models := []string{}
	if !p.cfg.CachePolicy.RequireManaged {
		return models
	}
	seen := map[string]bool{}
	for _, route := range p.cfg.CachePolicy.Routes {
		for _, model := range route.Models {
			if !seen[model] {
				seen[model] = true
				models = append(models, model)
			}
		}
	}
	sort.Strings(models)
	return models
}
