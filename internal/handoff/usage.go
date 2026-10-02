package handoff

import (
	"sort"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

// Usage is a read-only view. Viewing quota must not renew a chat's activity,
// reserve capacity, acknowledge notices, or move its credential binding.
type Usage struct {
	Protocol    int               `json:"protocol"`
	View        string            `json:"view"`
	Now         int64             `json:"now"`
	MaxAge      int64             `json:"max_age_seconds"`
	All         bool              `json:"all"`
	Provider    string            `json:"provider"`
	Model       string            `json:"model"`
	Current     string            `json:"current"`
	Active      []string          `json:"active"`
	LastUsed    int64             `json:"last_used"`
	Credentials []UsageCredential `json:"credentials"`
}

type UsageCredential struct {
	ID       string         `json:"id"`
	Alias    string         `json:"alias"`
	Provider string         `json:"provider"`
	Shared   []string       `json:"shared_with"`
	Windows  []quota.Window `json:"windows"`
}

// ConfirmUsage records the host's actual primary-request credential after
// admission. Auxiliary requests must not replace the chat's last-used identity.
func (e *Engine) ConfirmUsage(admission, auth string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.flights[admission]
	if !ok || f.Kind != "primary" || f.Auth != auth {
		return nil
	}
	b := e.state.Bindings[f.Key]
	if b == nil {
		return nil
	}
	if e.usageStarted == nil {
		e.usageStarted = map[string]bool{}
	}
	if e.usageStarted[admission] {
		return nil
	}
	e.usageStarted[admission] = true
	b.UsageAuth, b.UsageAt = auth, e.now().Unix()
	return e.save()
}

func (e *Engine) UsageView(capability, route string, all bool) (Usage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.broken != nil {
		return Usage{}, e.broken
	}
	hash := Digest(capability)
	for _, b := range e.state.Bindings {
		if b.Capability != hash || b.Route != route {
			continue
		}
		result := Usage{Protocol: 1, View: "usage", Now: e.now().Unix(), MaxAge: e.cfg.MaxAge,
			All: all, Provider: b.Provider, Model: b.Model, Current: b.UsageAuth,
			LastUsed: b.UsageAt, Active: []string{}, Credentials: []UsageCredential{}}
		active := map[string]bool{}
		for id, f := range e.flights {
			if f.Key == b.Key && f.Kind == "primary" && e.usageStarted[id] {
				active[f.Auth] = true
			}
		}
		for id := range active {
			result.Active = append(result.Active, id)
		}
		sort.Strings(result.Active)
		result.Credentials = e.usageCredentials(b.Provider, b.Model, all)
		return result, nil
	}
	return Usage{}, refuse("quota_unknown_session", "Session capability is not registered")
}

// AllUsage is independent of chat enrollment and never creates a binding.
// Its caller must first authenticate with CPA's ordinary API-key boundary.
func (e *Engine) AllUsage() (Usage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.broken != nil {
		return Usage{}, e.broken
	}
	return Usage{Protocol: 1, View: "usage", Now: e.now().Unix(), MaxAge: e.cfg.MaxAge,
		All: true, Active: []string{}, Credentials: e.usageCredentials("", "", true)}, nil
}

func (e *Engine) usageCredentials(provider, model string, all bool) []UsageCredential {
	result := Usage{Credentials: []UsageCredential{}}
	for _, c := range e.cfg.Credentials {
		if !all && c.Provider != provider {
			continue
		}
		row := UsageCredential{ID: c.ID, Alias: c.Alias, Provider: c.Provider, Shared: []string{}, Windows: []quota.Window{}}
		for _, other := range e.cfg.Credentials {
			if other.ID != c.ID && account(other) == account(c) {
				row.Shared = append(row.Shared, other.ID)
			}
		}
		for _, w := range e.state.Accounts[account(c)].Windows {
			if all || w.Applies(model) || w.Scope == "oauth" {
				row.Windows = append(row.Windows, w)
			}
		}
		sort.Strings(row.Shared)
		sort.Slice(row.Windows, func(i, j int) bool { return row.Windows[i].Key() < row.Windows[j].Key() })
		result.Credentials = append(result.Credentials, row)
	}
	sort.Slice(result.Credentials, func(i, j int) bool {
		a, b := result.Credentials[i], result.Credentials[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Alias != b.Alias {
			return a.Alias < b.Alias
		}
		return a.ID < b.ID
	})
	return result.Credentials
}
