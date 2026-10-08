package pacer

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

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
	for _, token := range []string{"", key} {
		req, _ := http.NewRequest("GET", endpoint, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := usageAuthClient.Do(req)
		if err != nil {
			return false, errors.New("usage authentication unavailable")
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		res.Body.Close()
		if readErr != nil {
			return false, errors.New("usage authentication unavailable")
		}
		if token == "" {
			if res.StatusCode != 401 && res.StatusCode != 403 {
				return false, errors.New("model-list authentication must be enabled")
			}
			continue
		}
		if res.StatusCode == 401 || res.StatusCode == 403 {
			return false, nil
		}
		var models struct {
			Object string          `json:"object"`
			Data   json.RawMessage `json:"data"`
		}
		if res.StatusCode != 200 || json.Unmarshal(body, &models) != nil || models.Object != "list" || len(models.Data) == 0 || models.Data[0] != '[' {
			return false, errors.New("unexpected model-list response")
		}
	}
	return true, nil
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
			policy = map[string]any{"protocol": 1, "contract_revision": 2, "endpoint": "/v1/plugins/pacer/cache-policy", "ttls": []string{"5m", "1h"}, "provider_routes": routes, "transports": []string{"http"}, "formats": []string{"claude", "openai-response"}, "enforced": true}
		}
		return resource(200, map[string]any{"protocol": 1, "mode": "off", "pacer": true, "cache_policy": policy, "auxiliary_ack": map[string]any{"version": 1, "kinds": []string{"compaction", "title", "generate"}}})
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
	} else {
		if req.Query.Get("auth") != "" || len(token) < 40 || len(token) > 128 {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
		var err error
		binding, err = p.chats.ByCapability(token, req.Query.Get("model"))
		if err != nil {
			return resource(401, map[string]string{"error": "unauthorized"})
		}
	}
	if req.Query.Get("view") != "usage" {
		windows, err := p.legacyWindows(binding.Account, binding.Model, false)
		if err != nil {
			return resource(503, map[string]string{"error": "quota_state_unavailable"})
		}
		return resource(200, map[string]any{"protocol": 1, "mode": "off", "primary_used": binding.PrimaryUsed, "consumed": binding.Consumed, "cursor": 0, "generation": binding.Generation, "alias": binding.Alias, "intent": "", "available": true, "windows": windows, "events": []any{}})
	}
	p.mu.Lock()
	creds := make([]credential, 0, len(p.creds))
	for _, c := range p.creds {
		creds = append(creds, c)
	}
	p.mu.Unlock()
	sort.Slice(creds, func(i, j int) bool { return creds[i].ID < creds[j].ID })
	rows := []map[string]any{}
	for _, c := range creds {
		if all != "true" && c.Provider != binding.Provider {
			continue
		}
		windows, err := p.legacyWindows(c.Account, binding.Model, all == "true")
		if err != nil {
			return resource(503, map[string]string{"error": "quota_state_unavailable"})
		}
		shared := []string{}
		for _, other := range creds {
			if other.ID != c.ID && other.Account == c.Account {
				shared = append(shared, other.ID)
			}
		}
		rows = append(rows, map[string]any{"id": c.ID, "alias": c.Alias, "provider": c.Provider, "shared_with": shared, "windows": windows})
	}
	active := []string{}
	if binding.Key != "" {
		p.mu.Lock()
		for _, t := range p.requests {
			if t.Key == binding.Key && t.Auth != "" && (t.Claim == nil || t.Claim.Kind == "primary") {
				active = append(active, t.Auth)
			}
		}
		p.mu.Unlock()
	}
	last := int64(0)
	if !binding.LastUsed.IsZero() {
		last = binding.LastUsed.Unix()
	}
	return resource(200, map[string]any{"protocol": 1, "view": "usage", "now": p.now().Unix(), "max_age_seconds": 3600, "all": all == "true", "provider": binding.Provider, "model": binding.Model, "current": binding.Auth, "active": active, "last_used": last, "credentials": rows})
}
