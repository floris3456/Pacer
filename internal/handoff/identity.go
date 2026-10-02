package handoff

import "github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"

// Identify folds auth-file aliases into the stable provider account learned from
// host-owned OAuth metadata. Neither the identity's plaintext nor tokens are stored.
func (e *Engine) Identify(auth, identity string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if identity == "" {
		return nil
	}
	if e.state.Identities == nil {
		e.state.Identities = map[string]string{}
	}
	for i, c := range e.cfg.Credentials {
		if c.ID != auth {
			continue
		}
		stable := Digest(c.Provider + ":" + identity)
		if c.Account == stable {
			return nil
		}
		previousIdentity := e.state.Identities[auth]
		old := account(c)
		e.cfg.Credentials[i].Account = stable
		next := account(e.cfg.Credentials[i])
		destination := e.state.Accounts[next]
		if destination.Windows == nil {
			destination.Windows = map[string]quota.Window{}
		}
		if destination.Samples == nil {
			destination.Samples = map[string]quota.Window{}
		}
		for key, w := range e.state.Accounts[old].Samples {
			if w.Observed > destination.Samples[key].Observed {
				destination.Samples[key] = w
			}
		}
		for key, w := range e.state.Accounts[old].Windows {
			prior, exists := destination.Windows[key]
			if !exists {
				destination.Windows[key] = w
				continue
			}
			selected := prior
			if w.Observed > prior.Observed {
				selected = w
			}
			// A rename must not erase a hold, even while the next provider read is pending.
			selected.Held = (prior.Held && !(w.Observed >= prior.Reset && w.Reset > prior.Reset+prior.Seconds/2)) || (w.Held && !(prior.Observed >= w.Reset && prior.Reset > w.Reset+w.Seconds/2))
			selected.Warned = prior.Warned || w.Warned
			selected.Reclaimed = prior.Reclaimed || w.Reclaimed
			destination.Windows[key] = selected
		}
		e.state.Accounts[next] = destination
		if previousIdentity == "" && e.state.Adaptive != nil {
			if m, ok := e.state.Adaptive.Models[old]; ok && e.state.Adaptive.Models[next].Generated < m.Generated {
				e.state.Adaptive.Models[next] = m
			}
			for _, ticket := range e.state.Adaptive.Tickets {
				if ticket.Account == old {
					ticket.Account = next
				}
			}
			for _, binding := range e.state.Bindings {
				if binding.Account == old {
					binding.Account = next
				}
				if limits, ok := binding.Departed[old]; ok {
					binding.Departed[next] = limits
					delete(binding.Departed, old)
				}
			}
			for j, alias := range e.cfg.Credentials {
				if account(alias) == old {
					e.cfg.Credentials[j].Account = stable
					e.state.Identities[alias.ID] = stable
				}
			}
		}
		e.state.Identities[auth] = stable
		for _, b := range e.state.Bindings {
			if b.Auth == auth && previousIdentity == "" {
				b.Account = next
			}
		}
		return e.save()
	}
	return nil
}
