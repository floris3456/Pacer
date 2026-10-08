package pacer

import (
	"encoding/json"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Discovery reads CPA-owned credentials locally. Only verified UUID/account IDs
// merge aliases; email labels are not account identity. Tokens are never retained.
func (p *Plugin) discover(force ...bool) {
	if p.Host == nil {
		p.discoveryFailed()
		return
	}
	p.discoverMu.Lock()
	defer p.discoverMu.Unlock()
	if !(len(force) > 0 && force[0]) && !p.lastDiscover.IsZero() && p.now().Sub(p.lastDiscover) < 5*time.Second {
		return
	}
	p.lastDiscover = p.now()
	raw, err := p.Host("host.auth.list", map[string]any{})
	if err != nil {
		p.discoveryFailed()
		return
	}
	var list struct {
		Files   []pluginapi.HostAuthFileEntry `json:"files"`
		Catalog *startupCatalog               `json:"catalog"`
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &list) != nil || list.Files == nil || len(list.Files) > maxCoverageInventory {
		p.discoveryFailed()
		return
	}
	if !p.acceptStartupCatalog(list.Catalog) {
		return
	}
	found := map[string]credential{}
	indices := map[string]bool{}
	for _, file := range list.Files {
		provider := file.Provider
		if provider == "" {
			provider = file.Type
		}
		if file.Disabled || (provider != "claude" && provider != "codex") {
			continue
		}
		// A compatible executor label is not proof of a subscription quota
		// adapter. Custom gateways keep CPA routing and never receive these
		// first-party analytics calls or lend their counters to this observer.
		if file.BaseURL != "" {
			u, err := url.Parse(file.BaseURL)
			if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
				continue
			}
			if provider == "claude" && (u.Host != "api.anthropic.com" || strings.TrimSuffix(u.Path, "/") != "") || provider == "codex" && (u.Host != "chatgpt.com" || strings.TrimSuffix(u.Path, "/") != "/backend-api/codex") {
				continue
			}
		}
		id := file.ID
		if id == "" {
			id = file.Name
		}
		if id == "" || file.AuthIndex == "" {
			p.coverageFailure("inventory_identity_missing", "")
			continue
		}
		if len(id) > 512 || len(file.AuthIndex) > 128 || indices[file.AuthIndex] {
			p.discoveryFailed()
			return
		}
		if _, exists := found[id]; exists {
			p.discoveryFailed()
			return
		}
		indices[file.AuthIndex] = true
		identity := "credential:" + id
		verified := false
		if raw, err = p.Host("host.auth.get", map[string]string{"auth_index": file.AuthIndex}); err == nil {
			var result struct {
				JSON struct {
					AccountUUID string `json:"account_uuid"`
					AccountID   string `json:"account_id"`
				} `json:"json"`
			}
			if len(raw) <= 1<<20 && json.Unmarshal(raw, &result) == nil {
				if provider == "claude" && validBillingMetadata(result.JSON.AccountUUID) {
					identity = "account:" + result.JSON.AccountUUID
					verified = true
				}
				if provider == "codex" && validBillingMetadata(result.JSON.AccountID) {
					identity = "account:" + result.JSON.AccountID
					verified = true
				}
			}
		}
		if !verified {
			p.coverageFailure("billing_mapping_unverified", provider+":"+tracker.Digest(provider+":"+identity))
		}
		alias := strings.TrimSpace(file.Label)
		if alias == "" {
			alias = provider + " " + tracker.Digest(id)[:8]
		}
		found[id] = credential{ID: id, Index: file.AuthIndex, Provider: provider, Account: provider + ":" + tracker.Digest(provider+":"+identity), Alias: alias, AccountVerified: verified, ExactIdentity: file.ID != ""}
	}
	groups := p.persistCoverageInventory(found)
	p.mu.Lock()
	changed := !maps.Equal(p.creds, found)
	p.creds = found
	p.coverageGroups = groups
	if changed {
		p.coverageGeneration++
		p.measurements = map[string]measurement.Report{}
	}
	p.mu.Unlock()
	for _, c := range found {
		if _, exists, err := p.observer.Status(c.Account); err == nil && !exists {
			_ = p.observer.Queue(c.Account, observation.Endpoint(c.Provider), observation.ReasonSeed, time.Time{})
		}
	}
}

func validBillingMetadata(v string) bool {
	if len(v) == 0 || len(v) > 256 {
		return false
	}
	for _, r := range v {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
