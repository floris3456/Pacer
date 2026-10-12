package pacer

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func validateUsageURL(raw string) (string, error) {
	if raw == "" {
		raw = "http://127.0.0.1:8317/v1/models"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.Port() == "" || u.Path != "/v1/models" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("usage_auth_url must be the loopback CPA /v1/models endpoint")
	}
	return u.String(), nil
}

var usageAuthClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("usage authentication cannot redirect") }}

// Compatibility for older clients. Validate against CPA's own inference boundary,
// not management credentials; this model-list request never runs inference.
func authenticateUsage(endpoint, key string) (bool, error) {
	accepted, _, err := modelList(endpoint, key, false)
	return accepted, err
}

func (p *Plugin) legacyWindows(account, model string, all bool) ([]map[string]any, error) {
	windows, err := p.observer.ReadWindows(account)
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for _, w := range windows {
		// Older clients require numeric remaining/reset; do not fabricate zeros
		// for missing fields. New management views retain the full partial record.
		if w.Remaining == nil || w.Reset.IsZero() || w.NotApplicable || w.Dormant || !w.FreshComplete(p.now(), time.Hour) || (!all && !w.Applies(model)) {
			continue
		}
		state := w.State(p.now())
		if w.BalanceAge(p.now()) >= time.Hour {
			state = "stale"
		}
		result = append(result, map[string]any{"scope": w.Scope, "seconds": w.Seconds, "remaining": *w.Remaining, "reset": float64(w.Reset.UnixNano()) / 1e9, "held": false, "observed": w.Usage.At.Unix(), "source": w.Usage.Source, "state": state, "reset_evidence": w.ResetEvidence})
	}
	return result, nil
}

func (p *Plugin) events(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	authorization := req.Headers.Get("Authorization")
	if authorization == "" {
		p.mu.Lock()
		defer p.mu.Unlock()
		var policy any = false
		if p.policy != nil {
			routes := []string{}
			for _, route := range p.cfg.CachePolicy.Routes {
				routes = append(routes, route.ID)
			}
			policy = map[string]any{"protocol": 1, "contract_revision": p.cachePolicyContractRevisionLocked(), "endpoint": "/v1/plugins/pacer/cache-policy", "ttls": []string{"5m", "1h"}, "provider_routes": routes, "transports": []string{"http"}, "formats": []string{"claude", "openai-response"}, "enforced": true}
		}
		return resource(200, map[string]any{"protocol": 1, "mode": "off", "pacer": true, "cache_policy": policy, "auxiliary_ack": map[string]any{"version": 1, "kinds": []string{"compaction", "title", "generate"}}, "client": clientDiscovery(), "api": p.clientAPILocked()})
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == authorization || len(token) == 0 || len(token) > 8192 {
		return resource(401, map[string]string{"error": "unauthorized"})
	}
	all := req.Query.Get("all")
	if all != "" && all != "true" && all != "false" {
		return resource(400, map[string]string{"error": "invalid_usage_scope"})
	}
	var binding tracker.Binding
	caller := ""
	if req.Query.Get("auth") == "api-key" && req.Query.Get("view") == "capabilities" {
		if req.Query.Get("v") != "1" {
			return clientError(422, "unsupported_version", "upgrade")
		}
		return p.clientCapabilities(token)
	}
	if req.Query.Get("auth") == "api-key" && req.Query.Get("view") == "prices" {
		return p.clientPrices(token)
	}
	if req.Query.Get("auth") == "api-key" {
		if req.Query.Get("view") != "usage" || all != "true" {
			return resource(400, map[string]string{"error": "usage_requires_all"})
		}
		p.mu.Lock()
		endpoint := p.cfg.UsageAuthURL
		p.mu.Unlock()
		accepted, err := authenticateUsage(endpoint, token)
		if err != nil {
			return resource(503, map[string]string{"error": "usage_auth_unavailable"})
		}
		if !accepted {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		caller = clientCaller(token)
	} else {
		if req.Query.Get("auth") != "" || len(token) < 40 || len(token) > 128 {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		var err error
		binding, err = p.chats.ByCapability(token, req.Query.Get("model"))
		if err != nil {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		caller = binding.Caller
	}
	p.mu.Lock()
	allowed := p.clientAllowedLocked(caller, "") && (binding.Auth == "" || p.clientAllowedLocked(caller, binding.Auth))
	p.mu.Unlock()
	if !allowed {
		return clientError(403, "account_access_denied", "contact_operator")
	}
	if req.Query.Get("view") != "usage" {
		windows, err := p.legacyWindows(binding.Account, binding.Model, false)
		if err != nil {
			return resource(503, map[string]string{"error": "quota_state_unavailable"})
		}
		return resource(200, map[string]any{"protocol": 1, "mode": "off", "primary_used": binding.PrimaryUsed, "consumed": binding.Consumed, "cursor": 0, "generation": binding.Generation, "alias": binding.Alias, "intent": "", "available": true, "windows": windows, "events": []any{}})
	}
	// Authenticate before waiting. Capture the cursor before reloading the
	// binding and windows, so concurrent changes wake the next subscription.
	var wait time.Duration
	if raw := req.Query.Get("wait_ms"); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 0 || ms > 20000 || len(req.Query.Get("cursor")) > 128 || req.Query.Get("v") != "1" {
			return resource(400, map[string]string{"error": "invalid_usage_wait"})
		}
		wait = time.Duration(ms) * time.Millisecond
	}
	cursor, ok := p.usageChanges.wait(req.Query.Get("cursor"), wait, p.workerDone)
	if !ok {
		return resource(503, map[string]string{"error": "usage_watch_unavailable"})
	}
	if wait > 0 && req.Query.Get("auth") == "api-key" {
		// Ordinary API-key revocation must also take effect while a read waits.
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
	}
	if req.Query.Get("auth") != "api-key" {
		// The chat may have switched accounts or lost its capability while held.
		// Keep this request scoped to the same caller's binding even if the
		// capability was removed and subsequently reused by another chat.
		current, err := p.chats.ByCapability(token, req.Query.Get("model"))
		if err != nil || current.Key != binding.Key {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		binding = current
		caller = current.Caller
	}
	p.mu.Lock()
	if !p.clientAllowedLocked(caller, "") || binding.Auth != "" && !p.clientAllowedLocked(caller, binding.Auth) {
		p.mu.Unlock()
		return clientError(403, "account_access_denied", "contact_operator")
	}
	creds := make([]credential, 0, len(p.creds)+len(p.usageCreds))
	for _, c := range p.creds {
		if p.clientAllowedLocked(caller, c.ID) {
			creds = append(creds, c)
		}
	}
	// Usage-only providers (Antigravity, OpenCode Go) have meters too.
	for _, c := range p.usageCreds {
		if observation.Endpoint(c.Provider) != "" && p.clientAllowedLocked(caller, c.ID) {
			creds = append(creds, c)
		}
	}
	p.mu.Unlock()
	sort.Slice(creds, func(i, j int) bool { return creds[i].ID < creds[j].ID })
	rows := []map[string]any{}
	for _, c := range creds {
		if all != "true" && c.Provider != binding.Provider {
			continue
		}
		var windows any
		if req.Query.Get("v") == "1" {
			// Clients of the declaration protocol: every window with its
			// state (stale, awaiting confirmation ...), as in X-Pacer-Quota.
			raw, err := p.observer.ReadWindows(c.Account)
			if err != nil {
				return resource(503, map[string]string{"error": "quota_state_unavailable"})
			}
			windows = clientWindows(raw, binding.Model, p.now())
		} else {
			legacy, err := p.legacyWindows(c.Account, binding.Model, all == "true")
			if err != nil {
				return resource(503, map[string]string{"error": "quota_state_unavailable"})
			}
			windows = legacy
		}
		shared := []string{}
		for _, other := range creds {
			if other.ID != c.ID && other.Account == c.Account {
				shared = append(shared, other.ID)
			}
		}
		row := map[string]any{"id": c.ID, "alias": c.Alias, "provider": c.Provider, "shared_with": shared, "windows": windows}
		if c.Provider == "codex" {
			row["credits"] = p.readCredits(c.Account).At(p.now())
		}
		rows = append(rows, row)
	}
	active := []string{}
	if binding.Key != "" {
		p.mu.Lock()
		for _, t := range p.requests {
			if t.Key == binding.Key && t.Auth != "" && p.clientAllowedLocked(caller, t.Auth) && (t.Claim == nil || t.Claim.Kind == "primary") {
				active = append(active, t.Auth)
			}
		}
		p.mu.Unlock()
	}
	last := int64(0)
	if !binding.LastUsed.IsZero() {
		last = binding.LastUsed.Unix()
	}
	now := p.now()
	view := map[string]any{"protocol": 1, "view": "usage", "now": now.Unix(), "now_ms": now.UnixMilli(), "cursor": cursor, "max_age_seconds": 3600, "all": all == "true", "provider": binding.Provider, "model": binding.Model, "current": binding.Auth, "active": active, "last_used": last, "credentials": rows}
	if binding.Key != "" && req.Query.Get("v") == "1" {
		view["next_use"] = p.nextUse(binding)
	}
	return resource(200, view)
}
