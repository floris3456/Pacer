package pacer

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Supplied only by the authenticated native host at registration. Request
// declarations cannot attest executor behavior or enable auxiliary dispatch.
type hostCachePolicySupport struct {
	ContractRevision int  `json:"contract_revision"`
	AuxiliaryNoCache bool `json:"auxiliary_no_cache"`
}

func (h hostCachePolicySupport) auxiliarySupported(schema int) bool {
	return schema >= 7 && h.ContractRevision >= 3 && h.AuxiliaryNoCache
}

func (p *Plugin) cachePolicyContractRevisionLocked() int {
	if p.hostCachePolicy.auxiliarySupported(p.hostSchema) {
		return 3
	}
	return 2
}

func (p *Plugin) admittedPolicyLocked(selected pluginapi.CachePolicy) bool {
	if selected.Protocol != 1 {
		return false
	}
	switch selected.Kind {
	case "primary", "warming":
		return selected.TTL == "5m" || selected.TTL == "1h"
	case "auxiliary":
		return selected.TTL == "" && p.policy != nil && p.hostCachePolicy.auxiliarySupported(p.hostSchema)
	case "native_compaction":
		return (selected.TTL == "5m" || selected.TTL == "1h") && p.claudeReplaySupportedLocked("native_compaction")
	default:
		return false
	}
}

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
	if !required || exists && matchingRoute && caller != "" && selected.Scope.CallerScope == caller && p.admittedPolicyLocked(selected) {
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

type clientCacheRoute struct {
	ProviderRoute string   `json:"provider_route"`
	Models        []string `json:"models"`
	TTLs          []string `json:"ttls"`
}

// Public route IDs are enrollment inputs, not credential IDs. Ambiguous model
// mappings remain explicit so clients cannot silently choose a policy scope.
func (p *Plugin) cacheRoutesLocked() []clientCacheRoute {
	routes := []clientCacheRoute{}
	if p.hostSchema < 7 || p.policy == nil {
		return routes
	}
	for _, configured := range p.cfg.CachePolicy.Routes {
		models := []string{}
		seen := map[string]bool{}
		for _, model := range configured.Models {
			if !seen[model] {
				models = append(models, model)
				seen[model] = true
			}
		}
		sort.Strings(models)
		routes = append(routes, clientCacheRoute{ProviderRoute: configured.ID, Models: models, TTLs: []string{"5m", "1h"}})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].ProviderRoute < routes[j].ProviderRoute })
	return routes
}
