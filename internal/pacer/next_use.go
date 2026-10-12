package pacer

import (
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Only CPA's last primary scheduler candidates are eligible for a preview.
// Reads never enroll a chat, reserve quota, select an account or refresh meters.
type routingPreviewInput struct {
	At              time.Time
	Caller          string
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

// Called under p.mu by both the scheduler and the read-only preview. This is
// deliberately the same evidence and scoring input construction for both paths.
func (p *Plugin) routingInputsLocked(input routingPreviewInput, now time.Time) ([]routing.Account, float64, []credential) {
	accounts := []routing.Account{}
	candidates := []credential{}
	expected := 0.0
	for _, id := range input.Candidates {
		c, ok := p.creds[id]
		if !ok || c.Provider != input.Provider || !p.clientAllowedLocked(input.Caller, c.ID) {
			continue
		}
		candidates = append(candidates, c)
		windows, err := p.observer.ReadWindows(c.Account)
		if err != nil {
			continue
		}
		account := routing.Account{Auth: c.ID, Account: c.Account, Budgets: routingBudgets(input.Provider, input.Model, windows, now)}
		if c.Provider == "codex" {
			credits, err := p.observer.ReadCredits(c.Account)
			account.QuotaExhaustionUncertain = err != nil || credits.At(now).State != "empty"
			// A successful meter read writes credits before its windows. New
			// empty credits must not combine with an older exhausted window
			// while that same read is replenishing the subscription allowance.
			for _, w := range windows {
				if w.Applies(input.Model) && !w.NotApplicable && w.Usage.At.UnixMilli() < credits.ReadMS {
					account.QuotaExhaustionUncertain = true
				}
			}
		}
		if report, ok := p.measurements[c.Account]; ok && now.Sub(report.AsOf) < time.Minute {
			selection := input.Selection
			if input.Managed {
				prior := report.Expected(measurement.ChatRef{CallerID: selection.Scope.CallerScope, InstallationID: selection.Scope.InstallationID, SessionID: selection.Scope.SessionID, ProviderRoute: selection.Scope.ProviderRoute, Provider: input.Provider, AccountID: c.Account, ContextEpoch: selection.ContextEpoch}, measurement.TTL(selection.TTL))
				if prior.Known && prior.USD > expected {
					expected = prior.USD
				}
			}
			if report.SevenDay.Known {
				account.Size = report.SevenDay.USDPerPercent
			}
			if report.FiveHour.Known {
				account.ShortValue = report.FiveHour.USDPerPercent
				account.RunwayUnknown = !report.WarmBurn.Bounded
			}
			if report.WarmBurn.Bounded {
				account.WarmBurn = report.WarmBurn.HighUSDPerHour
			}
		}
		accounts = append(accounts, account)
	}
	return accounts, expected, candidates
}

func (p *Plugin) nextUse(binding tracker.Binding) nextUseView {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	view := nextUseView{Version: 1, AtMS: now.UnixMilli(), ExpiresMS: now.Add(30 * time.Second).UnixMilli(), Mode: p.cfg.Routing.Mode, Provider: binding.Provider, Model: binding.Model, Reason: "unavailable"}
	if !p.cfg.Enabled || p.cfg.Routing.Mode == "off" {
		view.Reason = "routing_off"
		return view
	}
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
	accounts, expected, candidates := p.routingInputsLocked(input, now)
	d := routing.Pick(routing.Default(), now, routing.Chat{Account: current.Account, ProtectedUntil: current.ProtectedUntil, UnknownWarmth: current.UnknownWarmth, InFlight: active, ExpectedValue: expected}, accounts)
	view.Reason = d.Reason
	if !d.Handled || d.Account == current.Account || (d.Reason != "cold_move" && d.Reason != "forced_move") {
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
