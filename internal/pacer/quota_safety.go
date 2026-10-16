package pacer

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *Plugin) observeClaudeExtra(c credential, headers http.Header, at time.Time) {
	if c.Provider != "claude" || p.credits == nil {
		return
	}
	if extra, ok := observation.ClaudeExtraHeaders(headers, at); ok {
		_, err := p.credits.ObserveCredits(c.Account, extra)
		p.metricError(err)
	}
}

func (p *Plugin) observeClaudeExtraCaptured(c credential, headers http.Header, clocks map[string]time.Time) {
	if c.Provider != "claude" || p.credits == nil {
		return
	}
	captured := map[string]time.Time{}
	for name, at := range clocks {
		captured[strings.ToLower(name)] = at
	}
	groups := map[time.Time]http.Header{}
	for name, values := range headers {
		key := strings.ToLower(name)
		if key != "anthropic-ratelimit-unified-overage-status" && key != "anthropic-ratelimit-unified-overage-disabled-reason" {
			continue
		}
		at := captured[key]
		if at.IsZero() {
			continue
		}
		if groups[at] == nil {
			groups[at] = http.Header{}
		}
		for _, value := range values {
			groups[at].Add(name, value)
		}
	}
	times := make([]time.Time, 0, len(groups))
	for at := range groups {
		times = append(times, at)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for _, at := range times {
		p.observeClaudeExtra(c, groups[at], at)
	}
}

// This basic availability guard is independent of the pace router.
// It uses only observed Claude limits; Codex subscription limits alone cannot
// establish exhaustion because an account may still have paid credits.
func claudeExhausted(windows []observation.Window, model string, now time.Time) bool {
	for _, w := range windows {
		if w.Provider == "claude" && w.Applies(model) && !w.Dormant && w.Remaining != nil &&
			*w.Remaining == 0 && w.FreshComplete(now, time.Hour) {
			return true
		}
	}
	return false
}

// Prefer unused subscription allowance when overage is unknown, but never
// synthesize a denial unless extra capacity is also known to be unavailable.
func claudeUnavailable(windows []observation.Window, model string, now time.Time, extra observation.Credits, definitive bool) bool {
	extra = extra.At(now)
	if extra.State == "available" || extra.State == "unlimited" || definitive && extra.State != "empty" {
		return false
	}
	for _, w := range windows {
		// The meter records extra capacity before its quota windows. Do not
		// combine a new disabled flag with an older exhausted quota reading.
		if extra.State == "empty" && w.Usage.At.UnixMilli() < extra.ReadMS {
			continue
		}
		if claudeExhausted([]observation.Window{w}, model, now) {
			return true
		}
	}
	return false
}

// Only choose from the host's current candidates, never resurrect cooling,
// disabled, tried or model-incompatible credentials. Keep a usable binding;
// otherwise honor host priority and use a stable tie-break within that tier.
func (p *Plugin) pickClaudeQuota(req pluginapi.SchedulerPickRequest, t attempt) pluginapi.SchedulerPickResponse {
	if req.Model == "" || len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	for _, provider := range req.Providers {
		if provider != "claude" {
			return pluginapi.SchedulerPickResponse{}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observer == nil || p.chats == nil {
		return pluginapi.SchedulerPickResponse{}
	}
	key := chatKey(t.Caller, "claude", req.Model, t.Session, t.Claim)
	binding, _, err := p.chats.Get(key)
	if err != nil {
		return pluginapi.SchedulerPickResponse{}
	}
	seen := map[string]bool{}
	exhausted := false
	var selected *pluginapi.SchedulerAuthCandidate
	for i := range req.Candidates {
		candidate := &req.Candidates[i]
		c, known := p.creds[candidate.ID]
		if candidate.ID == "" || seen[candidate.ID] || !known || c.ID != candidate.ID || c.Account == "" ||
			c.Provider != "claude" || candidate.Provider != "claude" {
			return pluginapi.SchedulerPickResponse{}
		}
		seen[candidate.ID] = true
		if !p.clientAllowedLocked(t.Caller, c.ID) {
			continue
		}
		if p.cfg.Routing.Mode == "on" && p.settings.paused(c.Account) {
			continue
		}
		if weight, err := strconv.Atoi(candidate.Attributes["weight"]); err == nil && weight <= 0 {
			continue
		}
		windows, err := p.observer.ReadWindows(c.Account)
		if err != nil {
			return pluginapi.SchedulerPickResponse{}
		}
		if claudeUnavailable(windows, req.Model, p.now(), p.readCredits(c.Account), false) {
			exhausted = true
			continue
		}
		if selected == nil || candidate.ID == binding.Auth || selected.ID != binding.Auth &&
			(candidate.Priority > selected.Priority || candidate.Priority == selected.Priority && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if !exhausted || selected == nil {
		return pluginapi.SchedulerPickResponse{}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected.ID}
}

// Recheck immediately before admission: evidence may change after scheduling.
// Refusing here neither consumes a checkpoint nor sends a request upstream.
func (p *Plugin) exhaustedClaude(c credential, model string) pluginapi.RequestInterceptResponse {
	p.mu.Lock()
	observer, enabled := p.observer, p.cfg.Enabled
	p.mu.Unlock()
	if !enabled || c.Provider != "claude" || observer == nil {
		return pluginapi.RequestInterceptResponse{}
	}
	windows, err := observer.ReadWindows(c.Account)
	if err != nil || !claudeUnavailable(windows, model, p.now(), p.readCredits(c.Account), true) {
		return pluginapi.RequestInterceptResponse{}
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"type": "rate_limit_error", "code": "quota_exhausted",
		"message": "The selected Claude account has no remaining quota and no usable alternative was selected. Retry after quota resets or enable another Claude account.",
	}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusTooManyRequests,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "X-Should-Retry": {"false"}}, ResponseBody: body}
}
