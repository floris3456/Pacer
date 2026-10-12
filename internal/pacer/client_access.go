package pacer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Omitted means the existing shared pool. An explicit empty configuration grants
// nobody access. Only the operator supplies grants; client headers never do.
type ClientAccess struct {
	Grants []ClientGrant `yaml:"grants"`
}

type ClientGrant struct {
	CallerScope string   `yaml:"caller_scope"`
	AuthIDs     []string `yaml:"auth_ids"`
}

// This is supplied by CPA at registration, never read from plugin YAML or
// request headers. Without it account grants cannot survive a failed hook.
type requiredInterceptors struct {
	Version                 int    `json:"version"`
	PluginID                string `json:"plugin_id"`
	Required                bool   `json:"required"`
	RequestInterceptors     bool   `json:"request_interceptors"`
	NestedModelInterceptors bool   `json:"nested_model_interceptors"`
	RetryInterceptors       bool   `json:"retry_interceptors"`
	AuthPoolFiltering       bool   `json:"auth_pool_filtering"`
}

func (r requiredInterceptors) enforced() bool {
	return r.Version == 1 && r.PluginID == "pacer" && r.Required && r.RequestInterceptors && r.NestedModelInterceptors && r.RetryInterceptors
}

func (r requiredInterceptors) accountPoolEnforced() bool {
	return r.enforced() && r.AuthPoolFiltering
}

// Policy RPC scopes come from CPA's authenticated principal. Authorization is
// independent of a client's private policy capability and asserted checkpoint.
func (p *Plugin) authorizePolicyCall(method string, raw []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.ClientAccess == nil {
		return nil
	}
	var request struct {
		Scope  pluginapi.CachePolicyScope `json:"scope"`
		Policy pluginapi.CachePolicy      `json:"policy"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return pluginabi.NewError("pacer_cache_invalid_request", "Invalid cache policy request", 400)
	}
	caller := request.Scope.CallerScope
	if method == pluginabi.MethodCachePolicyAttempt {
		caller = request.Policy.Scope.CallerScope
	}
	if !p.clientAllowedLocked(caller, "") {
		return pluginabi.NewError("pacer_account_access_denied", "Account access is not granted to this authenticated caller", 403)
	}
	return nil
}

func validateClientAccess(access *ClientAccess) error {
	if access == nil {
		return nil
	}
	if len(access.Grants) > 256 {
		return errors.New("too many client access grants")
	}
	callers := map[string]bool{}
	for _, grant := range access.Grants {
		_, err := hex.DecodeString(grant.CallerScope)
		if err != nil || len(grant.CallerScope) != 64 || grant.CallerScope != strings.ToLower(grant.CallerScope) || callers[grant.CallerScope] || len(grant.AuthIDs) == 0 || len(grant.AuthIDs) > 256 {
			return errors.New("invalid client access grant")
		}
		callers[grant.CallerScope] = true
		ids := map[string]bool{}
		for _, id := range grant.AuthIDs {
			if !validID(id) || ids[id] {
				return errors.New("invalid granted credential ID")
			}
			ids[id] = true
		}
	}
	return nil
}

// This matches CPA's ordinary API-key provider. Other authentication providers
// must expose their host scope through an attested API before using these reads.
func clientCaller(key string) string {
	return tracker.Digest("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(key))
}

func denyClientAccess() pluginapi.RequestInterceptResponse {
	response := refuse("pacer_account_access_denied", "The selected account is not granted to this authenticated caller")
	response.StatusCode = 403
	return response
}

func unavailableClientAccess() pluginapi.RequestInterceptResponse {
	response := refuse("pacer_account_access_unavailable", "Account access could not be verified")
	response.StatusCode = 503
	return response
}

// Caller holds p.mu. Only operator grants and the host-authenticated caller may
// constrain selection. Copy IDs so the in-flight response cannot track later
// grant edits. A missing restriction is shared mode; an explicit empty pool
// grants no credentials. Provider/model eligibility remains the host's job.
func (p *Plugin) clientPoolLocked(caller string) *pluginapi.AuthPool {
	if p.cfg.ClientAccess == nil {
		return nil
	}
	pool := &pluginapi.AuthPool{Version: 1, AuthIDs: []string{}}
	for _, grant := range p.cfg.ClientAccess.Grants {
		if grant.CallerScope == caller {
			pool.AuthIDs = append(pool.AuthIDs, grant.AuthIDs...)
			break
		}
	}
	return pool
}

// Caller holds p.mu. auth == "" checks membership without enumerating accounts.
func (p *Plugin) clientAllowedLocked(caller, auth string) bool {
	if p.cfg.ClientAccess == nil {
		return true
	}
	for _, grant := range p.cfg.ClientAccess.Grants {
		if grant.CallerScope != caller {
			continue
		}
		for _, id := range grant.AuthIDs {
			if auth == "" || auth == id {
				return true
			}
		}
	}
	return false
}
