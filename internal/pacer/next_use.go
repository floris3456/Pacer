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
	// Warm: the chat's cache may still be warm, so NEXT USE waits until it is cold.
	Warm bool `json:"warm"`
	// NEXT USE: the account the chat moves to once its cache is cold, only
	// when that is another account.
	Auth   string `json:"auth,omitempty"`
	Alias  string `json:"alias,omitempty"`
	Forced bool   `json:"forced"`
	// FORCE USE: the account a forced move sends the chat to, "now" when its
	// account cannot serve, "soon" when a meter is near its floor.
	ForceAuth   string `json:"force_auth,omitempty"`
	ForceAlias  string `json:"force_alias,omitempty"`
	ForceReason string `json:"force_reason,omitempty"`
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
// meter inputs for both paths. chatAccount, when CPA did not offer it, is added
// as not offered so routing can tell a briefly unavailable account from one
// that cannot serve.
func (p *Plugin) routingInputsLocked(input routingPreviewInput, now time.Time, chatAccount string) ([]routing.Account, []credential) {
	accounts := []routing.Account{}
	candidates := []credential{}
	offered := false
	for _, id := range input.Candidates {
		c, _, ok := p.routingCredLocked(id)
		if !ok || c.Provider != input.Provider {
			continue
		}
		candidates = append(candidates, c)
		windows, err := p.observer.ReadWindows(c.Account)
		if err != nil {
			continue
		}
		offered = offered || c.Account == chatAccount
		accounts = append(accounts, routing.Account{Auth: c.ID, Account: c.Account, Budgets: routingBudgets(input.Provider, input.Model, windows, now), USDPerPercent: p.measuredValueLocked(c.Account, now), Paused: p.settings.paused(c.Account)})
	}
	if chatAccount != "" && !offered {
		var own *credential
		for _, inventory := range []map[string]credential{p.creds, p.usageCreds} {
			for _, c := range inventory {
				if c.Account == chatAccount && c.Provider == input.Provider && (own == nil || c.ID < own.ID) {
					c := c
					own = &c
				}
			}
		}
		if own != nil {
			if windows, err := p.observer.ReadWindows(own.Account); err == nil {
				accounts = append(accounts, routing.Account{Auth: own.ID, Account: own.Account, Budgets: routingBudgets(input.Provider, input.Model, windows, now), USDPerPercent: p.measuredValueLocked(own.Account, now), NotOffered: true, Paused: p.settings.paused(own.Account)})
			}
		}
	}
	return accounts, candidates
}

// warm reports whether the chat's prompt cache may still be warm on its
// account: a request is running, cache evidence protects it, or (without
// evidence) its last request is within the warm time setting.
func warm(b tracker.Binding, active bool, now time.Time, unknownWarmth time.Duration) bool {
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
	accounts, candidates := p.routingInputsLocked(input, now, current.Account)
	next, force := routing.Preview(p.routingConfigLocked(binding.Provider, now), now, current.Account, accounts, p.settings.ForceWarning)
	// While warm too: where the chat goes once it is cold.
	view.Reason, view.Warm = next.Reason, warm(current, active, now, p.settings.warmFor())
	alias := func(d routing.Decision) (string, string) {
		if !d.Handled || d.Account == current.Account || (d.Reason != routing.ColdMove && d.Reason != routing.ForcedMove) {
			return "", ""
		}
		for _, c := range candidates {
			if c.ID == d.Auth {
				return c.ID, c.Alias
			}
		}
		return "", ""
	}
	if view.Auth, view.Alias = alias(next); view.Auth != "" {
		view.Forced = next.Forced
	}
	if view.ForceAuth, view.ForceAlias = alias(force); view.ForceAuth != "" {
		view.ForceReason = "soon"
		if next.Forced {
			view.ForceReason = "now"
		}
	}
	return view
}
