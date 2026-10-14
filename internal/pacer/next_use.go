package pacer

import (
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Only CPA's last primary scheduler candidates are eligible for a preview.
// Reads never enroll a chat, reserve quota, select an account or refresh meters.
type routingPreviewInput struct {
	At              time.Time
	Provider, Model string
	Candidates      []string
	Selection       pluginapi.CachePolicy
	Managed         bool
}

type nextUseView struct {
	Version        int    `json:"v"`
	AtMS           int64  `json:"at_ms"`
	ExpiresMS      int64  `json:"expires_ms"`
	CandidatesAtMS int64  `json:"candidates_at_ms,omitempty"`
	Mode           string `json:"mode"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Reason         string `json:"reason"`
	Auth           string `json:"auth,omitempty"`
	Alias          string `json:"alias,omitempty"`
	Forced         bool   `json:"forced"`
}

const previewRetention = 24 * time.Hour

func (p *Plugin) rememberPreviewLocked(key string, input routingPreviewInput) {
	if p.previews == nil {
		p.previews = map[string]routingPreviewInput{}
	}
	// Bound a long-running shared host's in-memory advisory history.
	if _, exists := p.previews[key]; !exists && len(p.previews) >= 4096 {
		oldest := key
		at := input.At
		for k, previous := range p.previews {
			if previous.At.Before(at) {
				oldest, at = k, previous.At
			}
		}
		delete(p.previews, oldest)
	}
	p.previews[key] = input
}

// Called under p.mu by both the scheduler and the read-only preview: the same
// meter inputs for both paths.
func (p *Plugin) routingInputsLocked(input routingPreviewInput, now time.Time) ([]routing.Account, []credential) {
	accounts := []routing.Account{}
	candidates := []credential{}
	for _, id := range input.Candidates {
		c, ok := p.creds[id]
		if !ok || c.Provider != input.Provider {
			continue
		}
		candidates = append(candidates, c)
		windows, err := p.observer.ReadWindows(c.Account)
		if err != nil {
			continue
		}
		accounts = append(accounts, routing.Account{Auth: c.ID, Account: c.Account, Budgets: routingBudgets(input.Provider, input.Model, windows, now)})
	}
	return accounts, candidates
}

// unknownWarmth is how long a chat without confirmed cache activity counts as
// warm after its last primary request: the longest Claude cache TTL.
const unknownWarmth = time.Hour

// warm reports whether the chat's prompt cache may still be warm on its account.
func warm(b tracker.Binding, active bool, now time.Time) bool {
	return active || now.Before(b.ProtectedUntil) || (b.UnknownWarmth && now.Before(b.LastUsed.Add(unknownWarmth)))
}

func (p *Plugin) nextUse(binding tracker.Binding) nextUseView {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Clients know the modes off, dry-run and active: routing on is "active".
	view := nextUseView{Version: 1, AtMS: now.UnixMilli(), ExpiresMS: now.Add(30 * time.Second).UnixMilli(), Mode: "off", Provider: binding.Provider, Model: binding.Model, Reason: "unavailable"}
	if !p.cfg.Enabled || p.cfg.Routing.Mode != "on" {
		view.Reason = "routing_off"
		return view
	}
	view.Mode = "active"
	enabled := false
	for _, provider := range p.cfg.Routing.Providers {
		enabled = enabled || provider == binding.Provider
	}
	if !enabled {
		return view
	}
	input, ok := p.previews[binding.Key]
	if !ok || input.Provider != binding.Provider || input.Model != binding.Model || now.Sub(input.At) >= previewRetention {
		return view
	}
	view.CandidatesAtMS = input.At.UnixMilli()
	current, active, err := p.chats.Get(binding.Key)
	if err != nil || current.Key == "" {
		return view
	}
	accounts, candidates := p.routingInputsLocked(input, now)
	d := routing.Pick(routing.Default(), now, routing.Chat{Account: current.Account, Warm: warm(current, active, now)}, accounts)
	view.Reason = d.Reason
	if !d.Handled || d.Account == current.Account || (d.Reason != routing.ColdMove && d.Reason != routing.ForcedMove) {
		return view
	}
	for _, c := range candidates {
		if c.ID == d.Auth {
			view.Auth, view.Alias, view.Forced = c.ID, c.Alias, d.Forced
			break
		}
	}
	return view
}
