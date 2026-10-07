package pacer

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Discovery reads CPA-owned credentials locally. Only verified UUID/account IDs
// merge aliases; email labels are not account identity. Tokens are never retained.
func (p *Plugin) discover() {
	if p.Host == nil {
		return
	}
	p.discoverMu.Lock()
	defer p.discoverMu.Unlock()
	if !p.lastDiscover.IsZero() && p.now().Sub(p.lastDiscover) < 5*time.Second {
		return
	}
	p.lastDiscover = p.now()
	raw, err := p.Host("host.auth.list", map[string]any{})
	if err != nil {
		return
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	found := map[string]credential{}
	for _, file := range list.Files {
		provider := file.Provider
		if provider == "" {
			provider = file.Type
		}
		if file.Disabled || (provider != "claude" && provider != "codex") {
			continue
		}
		id := file.ID
		if id == "" {
			id = file.Name
		}
		if id == "" || file.AuthIndex == "" {
			continue
		}
		identity := "credential:" + id
		if raw, err = p.Host("host.auth.get", map[string]string{"auth_index": file.AuthIndex}); err == nil {
			var result struct {
				JSON struct {
					AccountUUID string `json:"account_uuid"`
					AccountID   string `json:"account_id"`
				} `json:"json"`
			}
			if json.Unmarshal(raw, &result) == nil {
				if provider == "claude" && result.JSON.AccountUUID != "" {
					identity = "account:" + result.JSON.AccountUUID
				}
				if provider == "codex" && result.JSON.AccountID != "" {
					identity = "account:" + result.JSON.AccountID
				}
			}
		}
		alias := strings.TrimSpace(file.Label)
		if alias == "" {
			alias = provider + " " + tracker.Digest(id)[:8]
		}
		found[id] = credential{ID: id, Index: file.AuthIndex, Provider: provider, Account: provider + ":" + tracker.Digest(provider+":"+identity), Alias: alias}
	}
	p.mu.Lock()
	p.creds = found
	p.mu.Unlock()
	for _, c := range found {
		if _, exists, err := p.observer.Status(c.Account); err == nil && !exists {
			_ = p.observer.Queue(c.Account, observation.Endpoint(c.Provider), observation.ReasonSeed, time.Time{})
		}
	}
}
