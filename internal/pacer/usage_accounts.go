package pacer

// Providers Pacer records only for the Usage Quota page (Antigravity, OpenCode
// Go and any other non-routed provider): their credentials are discovered
// separately from Claude/Codex, never enter routing, coverage or the forward
// ledger, and their requests go straight to the usage record. Their responses
// carry no quota, so the observation scheduler reads their meters: every 30
// minutes, shortly after a burst of requests ends and just after a reset (never
// more than once per account in 15 minutes).

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// usageOnlyProvider reports whether Pacer records a provider for the usage
// page only.
func usageOnlyProvider(provider string) bool {
	return provider != "" && provider != "claude" && provider != "codex" && len(provider) <= 64
}

// discoverUsageOnly keeps the credentials of usage-only providers. Their
// identity is the credential itself (CPA keeps one file per login; a
// re-login keeps the file name for Antigravity and the key digest for
// OpenCode Go).
func (p *Plugin) discoverUsageOnly(files []pluginapi.HostAuthFileEntry) {
	found := map[string]credential{}
	for _, file := range files {
		provider := file.Provider
		if provider == "" {
			provider = file.Type
		}
		provider = strings.ToLower(strings.TrimSpace(provider))
		if file.Disabled || !usageOnlyProvider(provider) {
			continue
		}
		id := file.ID
		if id == "" {
			id = file.Name
		}
		if id == "" || file.AuthIndex == "" || len(id) > 512 || len(file.AuthIndex) > 128 {
			continue
		}
		alias := strings.TrimSpace(file.Label)
		if alias == "" {
			alias = provider + " " + tracker.Digest(id)[:8]
		}
		found[id] = credential{ID: id, Index: file.AuthIndex, Provider: provider, Account: provider + ":" + tracker.Digest(provider+":credential:"+id), Alias: alias, ExactIdentity: file.ID != ""}
	}
	p.mu.Lock()
	p.usageCreds = found
	observer := p.observer
	p.mu.Unlock()
	if observer == nil {
		return
	}
	for _, c := range found {
		if observation.Endpoint(c.Provider) == "" {
			continue
		}
		if _, exists, err := observer.Status(c.Account); err == nil && !exists {
			_ = observer.Queue(c.Account, observation.Endpoint(c.Provider), observation.ReasonSeed, time.Time{})
		}
	}
}

// recordUsageOnly records one final request of a usage-only provider.
func (p *Plugin) recordUsageOnly(raw []byte, authID, authIndex, provider string) {
	p.mu.Lock()
	var c credential
	ok := false
	for _, x := range p.usageCreds {
		if x.Provider == provider && (authID != "" && x.ID == authID || authID == "" && x.Index == authIndex) {
			c, ok = x, true
		}
	}
	store, source, observer := p.recordStore, p.measurementSource, p.observer
	p.mu.Unlock()
	if !ok {
		return
	}
	if observer != nil && observation.Endpoint(c.Provider) != "" {
		// The request may have moved the meter: read it once the burst ends.
		if err := observer.Activity(c.Account, c.Provider, p.now()); err != nil {
			p.metricError(err)
		}
	}
	if store == nil || source == "" {
		return
	}
	cred := measurement.NativeCredential{Provider: c.Provider, AccountID: c.Account, CredentialRef: tracker.Digest(c.Provider + "\x00" + c.ID + "\x00" + c.Index), AuthID: c.ID, AuthIndex: c.Index}
	record, err := measurement.FromNative(raw, cred, source)
	if err != nil || !record.Final {
		return
	}
	e := historyreports.UsageOnlyEvent(record, p.now())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	changes, err := store.PutEvents(ctx, []historyreports.RecordEvent{e})
	if err != nil {
		p.metricError(err)
		return
	}
	p.usageLive.changed(changes)
}

// usageLabels names every native account Pacer knows (routed and usage-only).
func (p *Plugin) usageLabels() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	labels := map[string]string{}
	for _, set := range []map[string]credential{p.creds, p.usageCreds} {
		for _, c := range set {
			if c.Account != "" && c.Alias != "" {
				labels[c.Account] = c.Alias
			}
		}
	}
	return labels
}

// usageAuth is the part of a credential file the meter reads need.
type usageAuth struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	ProjectID   string `json:"project_id"`
	APIKey      string `json:"api_key"`
}

// meterRequest is the provider's quota read for one credential.
func meterRequest(provider string, auth usageAuth) (method, url string, headers map[string][]string, body []byte, ok bool) {
	switch provider {
	case "claude":
		if auth.AccessToken == "" {
			return "", "", nil, nil, false
		}
		return "GET", "https://api.anthropic.com/api/oauth/usage", map[string][]string{"Authorization": {"Bearer " + auth.AccessToken}, "Accept": {"application/json"}, "anthropic-beta": {"oauth-2025-04-20"}}, nil, true
	case "codex":
		if auth.AccessToken == "" {
			return "", "", nil, nil, false
		}
		h := map[string][]string{"Authorization": {"Bearer " + auth.AccessToken}, "Accept": {"application/json"}}
		if auth.AccountID != "" {
			h["ChatGPT-Account-Id"] = []string{auth.AccountID}
		}
		return "GET", "https://chatgpt.com/backend-api/wham/usage", h, nil, true
	case "antigravity":
		if auth.AccessToken == "" || auth.ProjectID == "" {
			return "", "", nil, nil, false
		}
		b, _ := json.Marshal(map[string]string{"project": auth.ProjectID})
		return "POST", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", map[string][]string{"Authorization": {"Bearer " + auth.AccessToken}, "Content-Type": {"application/json"}, "Accept": {"application/json"}, "User-Agent": {"antigravity/cli/1.0.13 (aidev_client; os_type=linux; arch=amd64)"}}, b, true
	case "opencode-go":
		if auth.APIKey == "" {
			return "", "", nil, nil, false
		}
		return "GET", "https://opencode.ai/zen/go/v1/usage", map[string][]string{"Authorization": {"Bearer " + auth.APIKey}, "Accept": {"application/json"}, "User-Agent": {"Pacer/1 (quota poller)"}}, nil, true
	}
	return "", "", nil, nil, false
}
