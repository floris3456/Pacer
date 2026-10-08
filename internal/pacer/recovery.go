package pacer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
)

// Additive schema-7 host callback wire; an older host is simply unsupported.
type cooldown struct {
	Scope          string    `json:"scope"`
	Model          string    `json:"model_key"`
	Classification string    `json:"classification"`
	Required       []string  `json:"required_windows"`
	FailureAt      time.Time `json:"failure_observed_at"`
	Revision       string    `json:"revision"`
	Clearable      bool      `json:"clearable"`
}
type recoveryEvidence struct {
	Reason   string    `json:"reason"`
	Source   string    `json:"source"`
	Epoch    string    `json:"epoch"`
	At       time.Time `json:"observed_at"`
	Positive bool      `json:"all_applicable_windows_positive"`
}

func evidenceForRecovery(provider string, c cooldown, windows []observation.Window, now time.Time) (recoveryEvidence, bool) {
	if !c.Clearable || c.Classification != "quota" || c.Revision == "" || c.FailureAt.IsZero() || len(c.Required) == 0 {
		return recoveryEvidence{}, false
	}
	required := map[string]bool{}
	for _, key := range c.Required {
		required[key] = false
	}
	if provider == "claude" {
		required["all/18000"] = false
		required["all/604800"] = false
	}
	at := now
	epochs := []string{}
	newCycle := false
	source := ""
	for _, w := range windows {
		key := fmt.Sprintf("%s/%d", w.Scope, w.Seconds)
		_, needed := required[key]
		if !needed && c.Scope != "credential" && !w.Applies(c.Model) {
			continue
		}
		if w.NotApplicable && !needed {
			continue
		}
		if !w.FreshComplete(now, time.Hour) || w.Dormant || w.NotApplicable || w.Remaining == nil || *w.Remaining <= 0 || !w.Usage.At.After(c.FailureAt) || !w.ResetEvidence.At.After(c.FailureAt) || w.ResetEvidence.Quality != observation.Absolute || w.Usage.At.After(now) || w.ResetEvidence.At.After(now) {
			return recoveryEvidence{}, false
		}
		if needed {
			required[key] = true
		}
		if w.Usage.At.Before(at) {
			at = w.Usage.At
		}
		if w.ResetEvidence.At.Before(at) {
			at = w.ResetEvidence.At
		}
		if needed && w.CycleAt.After(c.FailureAt) {
			newCycle = true
		}
		switch w.Usage.Source {
		case observation.HeaderSource:
			source = "inference_headers"
		case observation.APISource:
			source = "quota_endpoint"
		case observation.EventSource:
			source = "provider_event"
		default:
			return recoveryEvidence{}, false
		}
		epochs = append(epochs, key+":"+w.Cycle)
	}
	for _, present := range required {
		if !present {
			return recoveryEvidence{}, false
		}
	}
	if !newCycle || len(epochs) == 0 {
		return recoveryEvidence{}, false
	}
	sort.Strings(epochs)
	return recoveryEvidence{Reason: "applicable_new_cycle", Source: source, Epoch: tracker.Digest(strings.Join(epochs, "|")), At: at, Positive: true}, true
}

// Background-only, entirely local host operations. Never use administrative
// reset_cooldown as a fallback and never clear more than the exact CAS target.
func (p *Plugin) recoverCooldowns() {
	if p.Host == nil {
		return
	}
	p.mu.Lock()
	creds := []credential{}
	for _, c := range p.creds {
		creds = append(creds, c)
	}
	p.mu.Unlock()
	for _, c := range creds {
		raw, err := p.Host("host.quota.cooldowns.read", map[string]string{"auth_index": c.Index})
		if err != nil {
			continue
		}
		var result struct {
			Supported bool       `json:"supported"`
			Provider  string     `json:"provider"`
			Index     string     `json:"auth_index"`
			Cooldowns []cooldown `json:"cooldowns"`
		}
		if json.Unmarshal(raw, &result) != nil || !result.Supported || result.Index != c.Index || result.Provider != c.Provider {
			continue
		}
		windows, err := p.observer.ReadWindows(c.Account)
		if err != nil {
			continue
		}
		for _, restriction := range result.Cooldowns {
			evidence, ok := evidenceForRecovery(c.Provider, restriction, windows, p.now())
			if !ok {
				continue
			}
			_, _ = p.Host("host.quota.cooldowns.clear", map[string]any{"auth_index": c.Index, "scope": restriction.Scope, "model_key": restriction.Model, "revision": restriction.Revision, "evidence": evidence})
		}
	}
}
