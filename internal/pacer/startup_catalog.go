package pacer

// Additive host.auth.list provenance v1. This does not change the pinned SDK's
// physical types and cannot establish membership or either coverage domain.
type startupCatalog struct {
	Version    int    `json:"version"`
	Domain     string `json:"domain"`
	State      string `json:"state"`
	Generation uint64 `json:"generation"`
	Listener   string `json:"listener_state"`
}

// Called under discoverMu. Only genuine pre-enrollment lifecycle observations
// defer work; once any runtime/legacy catalog was observed, loss is a sticky gap.
func (p *Plugin) acceptStartupCatalog(c *startupCatalog) bool {
	p.mu.Lock()
	seen, generation := p.coverageStats.catalogSeen, p.coverageStats.catalogGeneration
	ready, waiting := c == nil, false // Old hosts retain all existing validation.
	if c != nil && c.Version == 1 {
		listenerPending := c.Listener == "registering" || c.Listener == "installing"
		listenerValid := listenerPending || c.Listener == "ready"
		switch c.Domain {
		case "cpa.auth.disk":
			waiting = c.State == "uninitialized" && c.Generation == 0 && listenerValid
		case "cpa.auth.runtime":
			waiting = listenerValid && (c.State == "uninitialized" && c.Generation == 0 || c.State == "loading" && c.Generation > 0 || c.State == "ready" && c.Generation > 0 && listenerPending)
			ready = c.State == "ready" && c.Generation > 0 && c.Listener == "ready" && (!seen || c.Generation >= generation)
		}
	}
	if ready {
		p.coverageStats.catalogSeen = true
		if c != nil {
			p.coverageStats.catalogGeneration = c.Generation
		}
		p.mu.Unlock()
		return true
	}
	if waiting && !seen && p.coverageStats.Errors == 0 {
		p.coverageStats.Reason = "runtime_catalog_starting"
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()
	p.discoveryFailed()
	return false
}
