package pacer

// Clients that know Pacer (the opencode plugin on any host) declare what they
// handle on every request with X-Pacer-Client. Pacer never asks a client for
// something it did not declare, so a client without the plugin simply gets
// CPA's ordinary behaviour. Declarations are per request, not per API key: one
// key is often shared by clients with and without the plugin.
//
//	X-Pacer-Client: v=1; caps=quota
//
// caps=quota: Pacer adds X-Pacer-Quota to the reply, the serving account's
// quota windows after this very response (base64url JSON, quotaReply).
// Compaction before an account switch stays with the X-Quota-Handoff claim
// (compact_on_switch), which only hosts that can compact send.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	clientHeader     = "X-Pacer-Client"
	quotaReplyHeader = "X-Pacer-Quota"
	clientVersion    = 1
)

// clientCaps is what one request's client declared. The zero value is a client
// without the plugin.
type clientCaps struct {
	Version int
	Quota   bool
}

// clientDeclaration parses X-Pacer-Client. Anything malformed or of an unknown
// version counts as no declaration: a plain client is never refused for it.
func clientDeclaration(h http.Header) clientCaps {
	raw := header(h, clientHeader)
	if raw == "" || len(raw) > 512 {
		return clientCaps{}
	}
	var out clientCaps
	for _, part := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "v":
			if strings.TrimSpace(value) == "1" {
				out.Version = 1
			}
		case "caps":
			for _, c := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(c), "quota") {
					out.Quota = true
				}
			}
		}
	}
	if out.Version != clientVersion {
		return clientCaps{}
	}
	return out
}

// quotaWindow is one window in X-Pacer-Quota and in the usage views for
// clients. Left is the % left; times are Unix milliseconds.
type quotaWindow struct {
	Scope   string   `json:"scope"`
	Seconds int64    `json:"seconds"`
	Left    *float64 `json:"left,omitempty"`
	ResetMS int64    `json:"reset_ms,omitempty"`
	// ResetQuality is "absolute" (the provider's timestamp) or "estimated"
	// (derived from a countdown).
	ResetQuality string `json:"reset_quality,omitempty"`
	// State: current, estimated, stale, dormant, partial, unknown,
	// awaiting-confirmation (the reset passed and no reading confirmed it yet),
	// ambiguous or conflicting.
	State string `json:"state"`
	// ReadMS is when the balance was read; Source says how (header, api, event).
	ReadMS  int64  `json:"read_ms,omitempty"`
	Source  string `json:"source,omitempty"`
	Applies bool   `json:"applies"`
}

type quotaReply struct {
	V        int           `json:"v"`
	Provider string        `json:"provider"`
	Auth     string        `json:"auth"`
	Alias    string        `json:"alias,omitempty"`
	AtMS     int64         `json:"at_ms"`
	Windows  []quotaWindow `json:"windows"`
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// clientWindows renders an account's windows for clients. Windows the provider
// reports as not applying to the account are left out.
func clientWindows(windows []observation.Window, model string, now time.Time) []quotaWindow {
	out := []quotaWindow{}
	for _, w := range windows {
		if w.NotApplicable {
			continue
		}
		v := quotaWindow{Scope: w.Scope, Seconds: w.Seconds, Left: w.Remaining, ResetMS: ms(w.Reset), State: windowState(w, now), ReadMS: ms(w.Usage.At), Source: w.Usage.Source, Applies: model == "" || w.Applies(model)}
		if !w.Reset.IsZero() {
			v.ResetQuality = w.ResetEvidence.Quality
		}
		if w.Dormant {
			v.ReadMS = ms(w.StatusEvidence.At)
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seconds != out[j].Seconds {
			return out[i].Seconds < out[j].Seconds
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// quotaReplyValue is the X-Pacer-Quota value for a reply served by c. It runs
// after the reply's own headers were observed, so it includes them.
func (p *Plugin) quotaReplyValue(observer *observation.Store, c credential, model string) (string, bool) {
	windows, err := observer.ReadWindows(c.Account)
	if err != nil {
		return "", false
	}
	now := p.now()
	raw, err := json.Marshal(quotaReply{V: clientVersion, Provider: c.Provider, Auth: c.ID, Alias: c.Alias, AtMS: now.UnixMilli(), Windows: clientWindows(windows, model, now)})
	if err != nil || len(raw) > 6<<10 {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(raw), true
}

// servedCredential finds the credential CPA picked, routed or usage-only.
func (p *Plugin) servedCredential(auth string) (credential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.creds[auth]; ok {
		return c, true
	}
	c, ok := p.usageCreds[auth]
	return c, ok
}

// clientDiscovery tells a client which declarations and views Pacer supports.
func clientDiscovery() map[string]any {
	return map[string]any{"version": clientVersion, "request_header": clientHeader, "caps": []string{"quota"}, "quota_header": quotaReplyHeader, "views": []string{"usage", "prices", "usage-watch", "capabilities"}}
}

// modelList asks CPA's own model list with the client's key. accepted is false
// when CPA refuses the key; an error means the check itself was not possible
// (including a CPA whose model list needs no key at all).
func modelList(endpoint, key string, details bool) (accepted bool, body []byte, err error) {
	for _, token := range []string{"", key} {
		target := endpoint
		if token != "" && details {
			target += "?details=true"
		}
		req, _ := http.NewRequest("GET", target, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := usageAuthClient.Do(req)
		if err != nil {
			return false, nil, errors.New("usage authentication unavailable")
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if readErr != nil {
			return false, nil, errors.New("usage authentication unavailable")
		}
		if token == "" {
			if res.StatusCode != 401 && res.StatusCode != 403 {
				return false, nil, errors.New("model-list authentication must be enabled")
			}
			continue
		}
		if res.StatusCode == 401 || res.StatusCode == 403 {
			return false, nil, nil
		}
		var models struct {
			Object string          `json:"object"`
			Data   json.RawMessage `json:"data"`
		}
		if res.StatusCode != 200 || json.Unmarshal(raw, &models) != nil || models.Object != "list" || len(models.Data) == 0 || models.Data[0] != '[' {
			return false, nil, errors.New("unexpected model-list response")
		}
		body = raw
	}
	return true, body, nil
}

// clientPrice is a model's price for clients: USD per million tokens, and
// where it comes from (manual, imported or models.dev), as on Model Settings.
type clientPrice struct {
	priceView
	Source string `json:"source"`
}

// clientPrices answers view=prices: Pacer's price for every model the
// client's key can use, so the client's cost display matches Pacer's usage.
func (p *Plugin) clientPrices(token string) pluginapi.ManagementResponse {
	p.mu.Lock()
	endpoint, reader := p.cfg.UsageAuthURL, p.reports
	p.mu.Unlock()
	accepted, raw, err := modelList(endpoint, token, true)
	if err != nil {
		return resource(503, map[string]string{"error": "usage_auth_unavailable"})
	}
	if !accepted {
		return resource(401, map[string]string{"error": "unauthorized"})
	}
	p.mu.Lock()
	allowed := p.clientAllowedLocked(clientCaller(token), "")
	p.mu.Unlock()
	if !allowed {
		return clientError(403, "account_access_denied", "contact_operator")
	}
	var list struct {
		Data []struct {
			ID        string   `json:"id"`
			OwnedBy   string   `json:"owned_by"`
			Providers []string `json:"providers"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &list) != nil || len(list.Data) > 5000 {
		return resource(503, map[string]string{"error": "usage_auth_unavailable"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), priceSyncWindow)
	defer cancel()
	prices, state, _, err := p.modelPrices(ctx)
	if err != nil {
		return resource(503, map[string]string{"error": "prices_unavailable"})
	}
	var imported historyreports.ForwardComparisonBook
	if reader != nil {
		if _, imported, err = reader.Imported(ctx); err != nil {
			return resource(503, map[string]string{"error": "prices_unavailable"})
		}
	}
	models := map[string]clientPrice{}
	for _, m := range list.Data {
		if m.ID == "" || len(m.ID) > 256 {
			continue
		}
		provider := m.OwnedBy
		if len(m.Providers) > 0 {
			provider = m.Providers[0]
		}
		if price, source, ok := historyreports.ModelPriceFor(imported, prices, provider, m.ID); ok {
			models[m.ID] = clientPrice{priceView: viewPrice(price), Source: source}
		}
	}
	return resource(200, map[string]any{"protocol": 1, "view": "prices", "unit": "usd_per_million_tokens", "revision": prices.Revision, "synced_at_ms": state.SyncedAtMS, "models": models})
}
