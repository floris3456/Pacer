package pacer

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// routingAction is the sole pace-decision -> CPA action boundary. eligible is
// external rollout authorization, not elapsed time or a configurable override.
// candidates is a value-only discovery snapshot taken under Plugin.mu. Neither
// it nor req's host-owned candidate slices/maps are mutated or retained here.
func routingAction(req pluginapi.SchedulerPickRequest, mode string, eligible bool, d routing.Decision, candidates []credential) pluginapi.SchedulerPickResponse {
	if mode != "active" || !eligible || req.Model == "" || !d.Handled || d.Auth == "" || d.Account == "" || len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	provider := req.Provider
	if provider == "" {
		provider = req.Candidates[0].Provider
	}
	if provider != "claude" && provider != "codex" {
		return pluginapi.SchedulerPickResponse{}
	}
	for _, p := range req.Providers {
		if p != provider {
			return pluginapi.SchedulerPickResponse{}
		}
	}
	known := make(map[string]credential, len(candidates))
	for _, c := range candidates {
		if c.ID == "" || c.Account == "" || c.Provider != provider {
			return pluginapi.SchedulerPickResponse{}
		}
		if _, duplicate := known[c.ID]; duplicate {
			return pluginapi.SchedulerPickResponse{}
		}
		known[c.ID] = c
	}
	seen := make(map[string]bool, len(req.Candidates))
	matched := false
	for _, c := range req.Candidates {
		k, ok := known[c.ID]
		if c.ID == "" || c.Provider != provider || !ok || seen[c.ID] {
			return pluginapi.SchedulerPickResponse{}
		}
		seen[c.ID] = true
		if c.ID == d.Auth {
			matched = k.Account == d.Account
		}
	}
	if !matched {
		return pluginapi.SchedulerPickResponse{}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: d.Auth}
}

// selectionIntent never treats an advisory/dry-run decision as authorization to
// skip forced-move compaction. Only the exact cold move actually emitted by this
// request's latest scheduler invocation qualifies. CPA can pick another auth on
// fallback/retry; that selection must keep the established forced contract.
func selectionIntent(t attempt, actual credential, model string) tracker.Selection {
	s := tracker.Selection{Auth: actual.ID, Account: actual.Account, Alias: actual.Alias, Provider: actual.Provider, Model: model, Forced: true}
	if t.RoutingChoice.Handled && !t.RoutingChoice.Reject && t.RoutingChoice.DelegateBuiltin == "" &&
		t.RoutingChoice.AuthID == actual.ID && t.Decision.Handled && !t.Decision.Forced && t.Decision.Reason == "cold_move" &&
		t.Decision.Auth == actual.ID && t.Decision.Account == actual.Account && actual.ID != "" && actual.Account != "" &&
		t.RoutingProvider == actual.Provider && t.Model == model && t.Key != "" &&
		t.PreviousAccount != "" && t.PreviousAccount != actual.Account && (t.Claim == nil || t.Claim.Kind == "primary") {
		s.Forced = false
	}
	return s
}
