package pacer

import (
	"net/http"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This is a client contract version, independent of the CPA ABI/schema and the
// existing quota wire version. Additive fields do not require a coordinated
// client/server release. Unavailable operations are never simulated by a client.
type clientFeature struct {
	Supported        bool   `json:"supported"`
	Enabled          bool   `json:"enabled"`
	Reason           string `json:"reason,omitempty"`
	ContractRevision int    `json:"contract_revision,omitempty"`
}

type clientAPI struct {
	Version      int                      `json:"version"`
	Build        buildinfo.Info           `json:"build"`
	HostSchema   int                      `json:"host_schema"`
	Features     map[string]clientFeature `json:"features"`
	Access       map[string]string        `json:"access"`
	Requirements struct {
		ManagedCacheModels []string           `json:"managed_cache_models"`
		ManagedKinds       []string           `json:"managed_kinds"`
		CacheRoutes        []clientCacheRoute `json:"cache_routes"`
	} `json:"requirements"`
}

// Caller holds p.mu. Public discovery describes availability, not authorization
// to another user's accounts, sessions, or checkpoint operations.
func (p *Plugin) clientAPILocked() clientAPI {
	policy := clientFeature{Supported: p.hostSchema >= 7, Enabled: p.hostSchema >= 7 && p.policy != nil, ContractRevision: p.cachePolicyContractRevisionLocked()}
	if !policy.Supported {
		policy.Reason = "host_integration_required"
	} else if !policy.Enabled {
		policy.Reason = "not_configured"
	}
	auxiliary := clientFeature{Supported: p.hostCachePolicy.auxiliarySupported(p.hostSchema), ContractRevision: 1}
	auxiliary.Enabled = auxiliary.Supported && policy.Enabled
	if !auxiliary.Supported {
		auxiliary.Reason = "host_integration_required"
	} else if !auxiliary.Enabled {
		auxiliary.Reason = "not_configured"
	}
	switching := clientFeature{Supported: true, Enabled: p.cfg.CompactOnSwitch}
	if !switching.Enabled {
		switching.Reason = "not_configured"
	}
	access := "shared"
	if p.cfg.ClientAccess != nil {
		access = "granted"
	}
	enforcement := "optional-interceptors"
	if p.hostGuard.enforced() {
		enforcement = "required-interceptors"
	}
	api := clientAPI{
		Version: 1, Build: buildinfo.Current(), HostSchema: p.hostSchema,
		Features: map[string]clientFeature{
			"quota":                 {Supported: true, Enabled: true},
			"watch":                 {Supported: true, Enabled: true},
			"account_switch":        switching,
			"account_access":        {Supported: p.hostGuard.accountPoolEnforced(), Enabled: p.cfg.ClientAccess != nil && p.hostGuard.accountPoolEnforced()},
			"required_cache_policy": {Supported: p.hostGuard.enforced() && policy.Supported, Enabled: p.cfg.CachePolicy.RequireManaged && p.hostGuard.enforced() && policy.Enabled},
			"cache_policy":          policy,
			"managed_auxiliary":     auxiliary,
		},
		Access: map[string]string{"mode": access, "enforcement": enforcement},
	}
	api.Requirements.ManagedCacheModels = p.requiredModelsLocked()
	api.Requirements.ManagedKinds = []string{"primary", "warming"}
	if auxiliary.Enabled {
		api.Requirements.ManagedKinds = append(api.Requirements.ManagedKinds, "auxiliary")
	}
	api.Requirements.CacheRoutes = p.cacheRoutesLocked()
	for _, kind := range []string{"keepalive", "native_compaction"} {
		supported := p.claudeReplaySupportedLocked(kind)
		feature := clientFeature{Supported: supported, Enabled: supported && policy.Enabled, ContractRevision: 1}
		if !supported {
			feature.Reason = "host_integration_required"
		} else if !feature.Enabled {
			feature.Reason = "not_configured"
		}
		api.Features[kind] = feature
	}
	s := p.hostCodexOrigin
	guarded := s.Version == 1 && s.PhysicalHTTP && s.NativeCompact && p.codexOrigins != nil && p.hostGuard.accountPoolEnforced()
	api.Features["codex_native_compaction"] = clientFeature{Supported: guarded, Enabled: guarded, ContractRevision: 1}
	start := clientFeature{Supported: windowStartSupported(p.hostWindowStart), ContractRevision: 1}
	start.Enabled = start.Supported && p.cfg.Enabled && p.cfg.QuotaWindowStart.Enabled && p.windowStarts != nil
	if !start.Supported {
		start.Reason = "host_integration_required"
	} else if !start.Enabled {
		start.Reason = "not_configured"
	}
	api.Features["claude_quota_window_start"] = start
	return api
}

func clientError(status int, code, action string) pluginapi.ManagementResponse {
	return resource(status, map[string]any{"error": map[string]any{
		"code": code, "action": action, "retryable": status == http.StatusServiceUnavailable,
	}})
}

// Capability discovery with a key reuses the ordinary CPA authentication
// boundary. It is read-only and never creates a policy or sends inference.
func (p *Plugin) clientCapabilities(token string) pluginapi.ManagementResponse {
	p.mu.Lock()
	endpoint := p.cfg.UsageAuthURL
	p.mu.Unlock()
	accepted, err := authenticateUsage(endpoint, token)
	if err != nil {
		return clientError(503, "authentication_unavailable", "wait")
	}
	if !accepted {
		return clientError(401, "unauthorized", "authenticate")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.clientAllowedLocked(clientCaller(token), "") {
		return clientError(403, "account_access_denied", "contact_operator")
	}
	return resource(200, p.clientAPILocked())
}
