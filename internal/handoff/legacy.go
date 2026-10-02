package handoff

import (
	"fmt"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
)

// LegacyWarnings describes only the credential selected for this request. It
// neither creates a chat binding nor claims that the client can compact. The
// stream adapter deduplicates these keys within each response; arbitrary clients
// do not provide a trustworthy chat identity for cross-request deduplication.
func (e *Engine) LegacyWarnings(auth, model string, requests ...adaptive.Request) []Event {
	if e.cfg.Adaptive.Shadow() {
		return e.ShadowWarnings(auth, model, requests...)
	}
	if e.cfg.Adaptive.Enforce() {
		return e.AdaptiveLegacy(auth, model, requests...)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.credential(auth)
	if !ok || e.broken != nil {
		return nil
	}
	var events []Event
	for _, w := range e.windows(account(c), model) {
		if w.NotApplicable || (!w.Warned && !w.Held) || e.now().Unix()-w.Observed > e.cfg.MaxAge || w.Reset <= e.now().Unix() {
			continue
		}
		label, kind := "five-hour", "warning"
		if w.Seconds == 604800 {
			label = "seven-day"
		}
		if w.Scope != "all" {
			label += " (" + w.Scope + ")"
		}
		if w.Held {
			kind = "handoff"
		}
		id := fmt.Sprintf("%s:%s:%d:%s", account(c), w.Key(), w.Cycle, kind)
		text := fmt.Sprintf("[CPA quota warning] Credential %s has %.1f%% of its %s quota remaining; resets %s.", c.Alias, w.Remaining, label, time.Unix(w.Reset, 0).UTC().Format(time.RFC3339))
		events = append(events, Event{ID: id, Kind: kind, Text: text})
	}
	return events
}
