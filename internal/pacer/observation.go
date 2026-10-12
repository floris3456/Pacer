package pacer

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func routingBudgets(provider, model string, windows []observation.Window, now time.Time) []routing.Budget {
	result := []routing.Budget{}
	shared := map[int64]bool{}
	for _, w := range windows {
		if w.Scope == "all" {
			shared[w.Seconds] = true
		}
		if !w.Applies(model) || w.NotApplicable {
			continue
		}
		b := routing.Budget{Seconds: w.Seconds, Reset: w.Reset, Known: w.FreshComplete(now, time.Hour) && !w.Dormant && w.Remaining != nil}
		if w.Remaining != nil {
			b.Remaining = *w.Remaining
		}
		result = append(result, b)
	}
	// Incomplete Claude headers cannot masquerade as a complete placement budget.
	// Codex legitimately reports arbitrary durations and sometimes only one window.
	if provider == "claude" {
		for _, duration := range []int64{18000, 604800} {
			if !shared[duration] {
				result = append(result, routing.Budget{Seconds: duration})
			}
		}
	}
	return result
}

func (p *Plugin) observe(requestID string, metadata map[string]any, headers http.Header, at time.Time) {
	auth := text(metadata, "selected_auth_id")
	if auth == "" {
		p.mu.Lock()
		auth = p.requests[requestID].Auth
		p.mu.Unlock()
	}
	p.observeAuth(auth, headers, at)
}

func (p *Plugin) observeAuth(auth string, headers http.Header, at time.Time) {
	if at.IsZero() {
		return
	}
	p.mu.Lock()
	c, ok := p.creds[auth]
	observer := p.observer
	p.mu.Unlock()
	if !ok || observer == nil {
		return
	}
	p.observeClaudeExtra(c, headers, at)
	samples := observation.Headers(c.Provider, headers, at)
	if len(samples) > 0 {
		p.storeObservations(c, samples)
	}
}

func (p *Plugin) observeCaptured(auth string, headers http.Header, captured map[string]time.Time) {
	p.mu.Lock()
	c, ok := p.creds[auth]
	observer := p.observer
	p.mu.Unlock()
	if !ok || observer == nil {
		return
	}
	p.observeClaudeExtraCaptured(c, headers, captured)
	samples := observation.HeadersCaptured(c.Provider, headers, captured)
	if len(samples) > 0 {
		p.storeObservations(c, samples)
	}
}

func (p *Plugin) startWorker() {
	p.workerOnce.Do(func() {
		// Mode marks run apart from the loop below, so slow provider reads never
		// leave a dry-run interval unproven.
		p.startDryRunHeartbeat()
		p.startUsageLive()
		p.workerWait.Add(1)
		go func() {
			defer p.workerWait.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-p.workerDone:
					return
				case <-ticker.C:
					p.discover()
					p.reconcile()
					p.recoverCooldowns()
					p.refreshMeasurements()
				}
			}
		}()
	})
}

// Reconciliation is exclusively a background operation. UI/inference paths
// observe or read SQLite; they never invoke this method or provider HTTP.
func (p *Plugin) reconcile() {
	if p.Host == nil || !p.refreshMu.TryLock() {
		return
	}
	defer p.refreshMu.Unlock()
	p.mu.Lock()
	stopped := p.stopped
	creds := make([]credential, 0, len(p.creds)+len(p.usageCreds))
	for _, c := range p.creds {
		creds = append(creds, c)
	}
	for _, c := range p.usageCreds {
		if observation.Endpoint(c.Provider) != "" {
			creds = append(creds, c)
		}
	}
	p.mu.Unlock()
	if stopped {
		return
	}
	for _, c := range creds {
		select {
		case <-p.workerDone:
			return
		default:
		}
		job, err := p.observer.Acquire(c.Account)
		if err != nil || job == nil {
			continue
		}
		p.retrieve(c, *job)
	}
}

func (p *Plugin) retrieve(c credential, job observation.Job) {
	status := 0
	headers := http.Header{}
	defer func() { _ = p.observer.Finish(job, status, headers) }()
	raw, err := p.Host("host.auth.get_runtime", map[string]string{"auth_index": c.Index})
	if err != nil {
		return
	}
	var runtime pluginapi.HostAuthGetRuntimeResponse
	if json.Unmarshal(raw, &runtime) != nil || runtime.Auth.Disabled || runtime.Auth.ID != c.ID {
		return
	}
	raw, err = p.Host("host.auth.get", map[string]string{"auth_index": c.Index})
	if err != nil {
		return
	}
	var auth struct {
		JSON usageAuth `json:"json"`
	}
	if json.Unmarshal(raw, &auth) != nil {
		status = 401
		return
	}
	method, url, requestHeaders, requestBody, ok := meterRequest(c.Provider, auth.JSON)
	if !ok {
		status = 401
		return
	}
	routed := c.Provider == "claude" || c.Provider == "codex"
	opRaw, err := p.Host("host.http.operation_open", map[string]any{})
	if err != nil {
		return
	}
	var op struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal(opRaw, &op) != nil || op.ID == "" {
		return
	}
	// Local host reads only, bracketing this one scheduled provider read.
	var sandwich *capacitySandwich
	if routed {
		sandwich = p.capacityBefore(c)
	}
	cancelled := make(chan struct{})
	timer := time.AfterFunc(10*time.Second, func() { _, _ = p.Host("host.http.cancel", map[string]string{"operation_id": op.ID}); close(cancelled) })
	call := map[string]any{"operation_id": op.ID, "method": method, "url": url, "headers": requestHeaders}
	if requestBody != nil {
		call["body"] = requestBody
	}
	raw, err = p.Host("host.http.do", call)
	if !timer.Stop() {
		<-cancelled
	}
	at := p.now()
	if err != nil {
		return
	}
	if routed {
		p.capacityAfter(sandwich)
	}
	var response pluginapi.HTTPResponse
	if json.Unmarshal(raw, &response) != nil {
		return
	}
	status, headers = response.StatusCode, response.Headers
	if status != 200 {
		return
	}
	windows, err := observation.API(c.Provider, response.Body, at)
	if err != nil {
		return
	}
	if (c.Provider == "codex" || c.Provider == "claude") && p.credits != nil {
		credits := observation.CodexCredits(response.Body, at)
		if c.Provider == "claude" {
			credits = observation.ClaudeExtraUsage(response.Body, at)
		}
		changed, err := p.credits.ObserveCredits(c.Account, credits)
		p.metricError(err)
		if changed {
			p.usageChanges.notify()
		}
	}
	p.storeObservations(c, windows)
	if routed {
		p.capacitySample(sandwich, at, windows)
	}
}
