package pacer

import (
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/windowstart"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Operator-only configuration. Clients cannot request or multiply starts.
type WindowStartConfig struct {
	Enabled bool   `yaml:"enabled"`
	Model   string `yaml:"model"`
}

func windowStartSupported(c pluginapi.QuotaWindowStartCapability) bool {
	return c.Version == 1 && c.Claude && c.ExactAccount && c.SingleAttempt && c.NoCache
}

func validateWindowStart(c *WindowStartConfig, enabled bool, schema int, host pluginapi.QuotaWindowStartCapability) error {
	if c.Model == "" {
		c.Model = "claude-haiku-4-5-20251001"
	}
	if !pluginapi.QuotaWindowStartModelAllowed(c.Model) {
		return errors.New("quota_window_start.model must be a direct Claude Haiku model ID")
	}
	if c.Enabled && (!enabled || schema < 7 || !windowStartSupported(host)) {
		return errors.New("quota_window_start requires enabled Pacer and an attested single-attempt host")
	}
	return nil
}

func (p *Plugin) startWindowWorker() {
	p.mu.Lock()
	enabled := p.cfg.QuotaWindowStart.Enabled
	p.mu.Unlock()
	if !enabled {
		return
	}
	p.workerWait.Add(1)
	go func() {
		defer p.workerWait.Done()
		tick := time.NewTicker(windowstart.MinStartGap)
		defer tick.Stop()
		for {
			select {
			case <-p.workerDone:
				return
			case <-tick.C:
				p.startDormantWindows()
			}
		}
	}()
}

// Caller holds p.mu. Treat all aliases of an active billing account as busy.
func (p *Plugin) windowAccountAllowedLocked(c credential) bool {
	if p.stopped || !p.cfg.Enabled || !p.cfg.QuotaWindowStart.Enabled || p.windowStarts == nil || p.Host == nil || c.Provider != "claude" || !c.AccountVerified || !c.ExactIdentity {
		return false
	}
	current, present := p.creds[c.ID]
	if !present || current != c {
		return false
	}
	if p.cfg.ClientAccess != nil {
		allowed := false
		for _, grant := range p.cfg.ClientAccess.Grants {
			if slices.Contains(grant.AuthIDs, c.ID) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	for _, a := range p.requests {
		if p.creds[a.Auth].Account == c.Account || a.Served.Account == c.Account {
			return false
		}
	}
	return true
}

// This separate worker cannot delay quota polling or any user's inference.
func (p *Plugin) startDormantWindows() {
	if !p.windowStartMu.TryLock() {
		return
	}
	defer p.windowStartMu.Unlock()
	p.mu.Lock()
	var candidates []credential
	for _, c := range p.creds {
		if p.windowAccountAllowedLocked(c) {
			candidates = append(candidates, c)
		}
	}
	model := p.cfg.QuotaWindowStart.Model
	p.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if seen[c.Account] {
			continue
		}
		seen[c.Account] = true
		now := p.now()
		windows, err := p.observer.ReadWindows(c.Account)
		if err != nil {
			continue
		}
		extra := p.readCredits(c.Account)
		if windowstart.Eligible(windows, extra, model, now) != "" {
			continue
		}
		raw, err := p.Host("host.http.operation_open", map[string]any{})
		var op struct {
			ID string `json:"operation_id"`
		}
		if err != nil || json.Unmarshal(raw, &op) != nil || op.ID == "" {
			return
		}
		p.mu.Lock()
		allowed := p.windowAccountAllowedLocked(c)
		claimed := false
		if allowed {
			now = p.now()
			// Local evidence may have changed while opening the host operation.
			// Re-read a coherent snapshot immediately before the durable claim.
			windows, err = p.observer.ReadWindows(c.Account)
			extra = p.readCredits(c.Account)
			if err == nil && windowstart.Eligible(windows, extra, model, now) == "" {
				claimed, err = p.windowStarts.Claim(c.Account, time.UnixMilli(extra.ReadMS), now)
			}
		}
		p.mu.Unlock()
		if err != nil || !claimed {
			_, _ = p.Host("host.http.cancel", map[string]string{"operation_id": op.ID})
			continue
		}
		p.usageChanges.notify()
		if err := p.captureUsageCredential(pluginapi.QuotaWindowStartRequestID(op.ID), c); err != nil {
			p.metricError(p.windowStarts.Finish(c.Account, now, p.now(), windowstart.Result{}))
			_, _ = p.Host("host.http.cancel", map[string]string{"operation_id": op.ID})
			p.usageChanges.notify()
			return
		}
		p.executeWindowStart(c, model, op.ID, now)
		return // At most one account per tick, also enforced durably across restarts.
	}
}

func (p *Plugin) executeWindowStart(c credential, model, operation string, started time.Time) {
	finished := make(chan struct{})
	cancelled := make(chan struct{})
	go func() {
		defer close(cancelled)
		select {
		case <-p.workerDone:
			_, _ = p.Host("host.http.cancel", map[string]string{"operation_id": operation})
		case <-finished:
		}
	}()
	raw, err := p.Host("host.claude.quota_window_start", pluginapi.HostQuotaWindowStartRequest{
		Version: 1, OperationID: operation, AuthID: c.ID, AccountKey: c.Account, Model: model,
	})
	close(finished)
	<-cancelled
	_, _ = p.Host("host.http.cancel", map[string]string{"operation_id": operation})
	var response pluginapi.HostQuotaWindowStartResponse
	var evidence struct {
		Version    int   `json:"version"`
		Dispatched *bool `json:"dispatched"`
		StatusCode *int  `json:"status_code"`
	}
	// Any malformed/failed callback might already have sent bytes. Do not retry.
	result := windowstart.Result{Dispatched: true}
	if err == nil && len(raw) <= 64<<10 && json.Unmarshal(raw, &evidence) == nil && evidence.Version == 1 && evidence.Dispatched != nil && evidence.StatusCode != nil &&
		json.Unmarshal(raw, &response) == nil && (response.StatusCode == 0 || response.StatusCode >= 100 && response.StatusCode <= 599) && (response.Dispatched || response.StatusCode < 200 || response.StatusCode >= 300) {
		result.Dispatched, result.HTTPStatus = response.Dispatched, response.StatusCode
		result.RetryAfter = response.Headers.Get("Retry-After")
		// Diagnostics are a finite vocabulary, never arbitrary provider text.
		switch response.Reason {
		case "invalid_request", "unsupported_runtime", "credential_unavailable", "cancelled", "upstream_error", "response_incomplete",
			"http_rate_limited", "rate_limit_error", "spend_limit_reached", "authentication_error", "permission_error", "invalid_request_error", "not_found_error", "overloaded_error", "api_error":
			result.Reason = response.Reason
		}
		if result.HTTPStatus == 429 && (result.Reason == "" || result.Reason == "upstream_error" || result.Reason == "response_incomplete") {
			result.Reason = "http_rate_limited"
		}
		if result.Dispatched && result.HTTPStatus >= 200 && result.HTTPStatus < 300 {
			// A credential ID may be re-used by discovery while this request runs.
			// The response belongs to the captured billing account, not its replacement.
			p.storeObservations(c, observation.Headers(c.Provider, response.Headers, p.now()))
		}
	}
	p.metricError(p.windowStarts.Finish(c.Account, started, p.now(), result))
	// Preserve the observer's shared endpoint pacing and Retry-After budgets.
	p.metricError(p.observer.Queue(c.Account, observation.Endpoint(c.Provider), observation.ReasonManual, time.Time{}))
	p.usageChanges.notify()
}

func (p *Plugin) windowStartView(c credential) map[string]any {
	p.mu.Lock()
	enabled, model, store := p.cfg.QuotaWindowStart.Enabled && p.cfg.Enabled, p.cfg.QuotaWindowStart.Model, p.windowStarts
	p.mu.Unlock()
	view := map[string]any{"enabled": false, "state": "disabled"}
	if c.Provider != "claude" {
		view["reason"] = "unsupported_provider"
		return view
	}
	if !enabled || store == nil {
		return view
	}
	view["enabled"] = true
	if !c.AccountVerified || !c.ExactIdentity {
		view["state"], view["reason"] = "blocked", "account_identity_unverified"
		return view
	}
	a, err := store.Read(c.Account)
	if err != nil {
		view["state"], view["reason"] = "blocked", "ledger_unavailable"
		return view
	}
	view["state"] = a.State
	view["max_attempts"] = windowstart.MaxAttempts
	if !a.Started.IsZero() {
		view["attempt"] = a
	}
	if a.State == "confirmed" && a.ConfirmedReset.After(p.now()) {
		return view
	}
	if a.State == "awaiting_confirmation" || a.State == "unconfirmed" {
		view["reason"] = "provider_confirmation_required"
		return view
	}
	if a.State == "blocked" {
		view["reason"] = a.Reason
		return view
	}
	windows, err := p.observer.ReadWindows(c.Account)
	if err != nil {
		view["reason"] = "fresh_quota_required"
	} else if reason := windowstart.Eligible(windows, p.readCredits(c.Account), model, p.now()); reason != "" {
		view["reason"] = reason
	} else if a.State == "retry_wait" {
		view["reason"] = a.Reason
	}
	return view
}
