package bridge

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const discoverInterval = 30 * time.Second
const discoverForcedGap = 5 * time.Second

// discover syncs the engine's credentials with CPA's auth list (metadata only,
// no tokens). Periodic calls run at most every 30 s; forced calls (an unknown
// host-selected credential) at most every 5 s. A failed host call keeps the
// current set.
func (p *Plugin) discover(force bool) {
	if p.Engine == nil || !p.Engine.Discovery() || p.Host == nil || p.stopped.Load() {
		return
	}
	p.discoverMu.Lock()
	defer p.discoverMu.Unlock()
	now := time.Now()
	if (!force && now.Sub(p.lastDiscover) < discoverInterval) || (force && now.Sub(p.lastDiscover) < discoverForcedGap) {
		return
	}
	p.lastDiscover = now
	raw, err := p.Host(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	found := []handoff.Discovered{}
	for _, f := range list.Files {
		if f.Disabled {
			continue
		}
		provider := f.Provider
		if provider == "" {
			provider = f.Type
		}
		id := f.ID
		if id == "" {
			id = f.Name
		}
		label := strings.TrimSpace(f.Label)
		if label == "" {
			label = strings.TrimSpace(f.Email)
		}
		found = append(found, handoff.Discovered{ID: id, Index: f.AuthIndex, Provider: provider, Label: label})
	}
	added, removed := p.Engine.SyncCredentials(found)
	if len(added) > 0 || len(removed) > 0 {
		// CPA's text log omits structured fields; keep the IDs in the message.
		p.notificationLog("info", "credential set follows CPA auths: added=["+strings.Join(added, ",")+"] removed=["+strings.Join(removed, ",")+"]",
			map[string]any{"notifications_enabled": !p.notificationsMuted.Load()})
	}
}
