package handoff

import (
	"sort"
	"strings"
)

// Discovered is one credential reported by CPA's host auth list. Only routing
// metadata is used; tokens are never read here.
type Discovered struct {
	ID       string
	Index    string
	Provider string
	Label    string
}

// Discovery reports whether the effective credential set follows CPA's auth list.
func (e *Engine) Discovery() bool { return e.cfg.DiscoverCredentials }

// SyncCredentials makes the effective credential set equal to CPA's current
// enabled Claude/Codex auths. Configured entries keep their alias, rank and
// account seed; auths that are disabled or deleted in CPA leave the set. Quota
// state (accounts, windows, identities, bindings, adaptive models) is never
// deleted, so a credential that returns resumes with its history.
func (e *Engine) SyncCredentials(found []Discovered) (added, removed []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.DiscoverCredentials {
		return nil, nil
	}
	configured := map[string]Credential{}
	for _, c := range e.static {
		configured[c.ID] = c
	}
	current := map[string]Credential{}
	for _, c := range e.cfg.Credentials {
		current[c.ID] = c
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Provider != found[j].Provider {
			return found[i].Provider < found[j].Provider
		}
		return found[i].ID < found[j].ID
	})
	next := []Credential{}
	seen := map[string]bool{}
	for _, d := range found {
		provider := strings.ToLower(strings.TrimSpace(d.Provider))
		if d.ID == "" || d.Index == "" || seen[d.ID] || (provider != "claude" && provider != "codex") {
			continue
		}
		seen[d.ID] = true
		c, ok := configured[d.ID]
		if !ok || c.Provider != provider {
			alias := strings.TrimSpace(d.Label)
			if alias == "" {
				alias = d.ID
			}
			c = Credential{ID: d.ID, Provider: provider, Alias: alias, Rank: 1, Account: provider + "-" + d.Index}
		}
		c.Index = d.Index
		if known := e.state.Identities[c.ID]; known != "" {
			c.Account = known
		} else if prior, ok := current[c.ID]; ok && prior.Provider == c.Provider {
			c.Account = prior.Account
		}
		if _, ok := current[c.ID]; !ok {
			added = append(added, c.ID)
		}
		next = append(next, c)
	}
	for id := range current {
		if !seen[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	e.cfg.Credentials = next
	return added, removed
}
