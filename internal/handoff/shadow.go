package handoff

import (
	"fmt"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
)

// Shadow observes CPA's actual choice. It never chooses, holds, switches or
// requests a checkpoint, even with missing telemetry or depleted credentials.
func (e *Engine) observeSelection(caller, provider, model string, selected []string, q *Claim) (Flight, error) {
	if len(selected) != 1 {
		return Flight{}, fmt.Errorf("shadow needs the host-selected credential")
	}
	c, ok := e.credential(selected[0])
	if !ok || c.Provider != provider {
		return Flight{}, fmt.Errorf("unknown observed credential")
	}
	f := Flight{Auth: c.ID, Account: account(c), Alias: c.Alias}
	if q == nil {
		return f, nil
	}
	if caller == "" || !q.Valid() {
		return Flight{}, fmt.Errorf("invalid observation identity")
	}
	key := Digest(caller + "\x00" + q.Installation + "\x00" + q.Session + "\x00" + provider + "\x00" + q.Route)
	b := e.state.Bindings[key]
	if b != nil && b.Capability != q.Capability {
		return Flight{}, fmt.Errorf("observation capability conflict")
	}
	if b == nil {
		b = &Binding{Key: key, Route: q.Route, Capability: q.Capability, Provider: provider, Generation: 1, Seen: map[string]bool{}}
		e.state.Bindings[key] = b
	}
	if b.Account != "" && b.Account != f.Account {
		b.Generation++
	}
	b.Account, b.Auth, b.Alias, b.Model = f.Account, c.ID, c.Alias, model
	b.Auto, b.Last = true, e.now().Unix() // Estimate a manual summary reserve as well.
	if e.pending != nil && q.Kind != "compaction" {
		b.Expected = *e.pending
	}
	if q.Kind == "primary" {
		b.PrimaryUsed, b.SummaryReady = true, false
	}
	if q.Checkpoint != "" {
		b.Consumed = q.Checkpoint
	}
	f.Key, f.Generation, f.Kind = key, b.Generation, q.Kind
	f.Admission = "shadow:" + key + ":" + q.Request + ":" + c.ID
	e.flights[f.Admission] = f
	e.reconcileShadow(b)
	return f, e.save()
}

func (e *Engine) shadowWarnings(c Credential, model string, r adaptive.Request) []Event {
	var out []Event
	for _, w := range e.windows(account(c), model) {
		if w.NotApplicable || e.now().Unix()-w.Observed > e.cfg.MaxAge || w.Reset <= e.now().Unix() {
			continue
		}
		p := e.state.Adaptive.Project(account(c), e.windowBudget(w), e.activeSessions(account(c)), e.cfg.Adaptive, e.now().Unix())
		high, _, known := e.state.Adaptive.Cost(account(c), e.windowBudget(w), r, e.now().Unix())
		warning, urgent := w.Warned || w.Held, w.Held
		if known {
			headroom := p.Headroom + e.state.Adaptive.PendingSavings(account(c), e.windowBudget(w), e.now().Unix())
			urgent = urgent || headroom < high
			warning = warning || headroom <= high*e.cfg.Adaptive.Lead
		}
		if !warning && !urgent {
			continue
		}
		kind := "warning"
		if urgent {
			kind = "reminder"
		}
		// Calibration cannot delay the established warning thresholds. Its
		// conservative forecast can bring a manual reminder forward.
		text := e.adaptiveText(c.Alias, w, p, urgent)
		text = strings.ReplaceAll(text, "for the handoff", "for a summary")
		text += " Shadow mode: quota-triggered compaction and switching are off. In OpenCodePlus use /compact, or start a new chat; compaction does not reset quota."
		out = append(out, Event{ID: fmt.Sprintf("shadow:%s:%s:%d:%s", account(c), w.Key(), w.Cycle, kind), Kind: kind, Text: text})
	}
	return out
}

func (e *Engine) reconcileShadow(b *Binding) {
	b.Intent, b.QuotaHeld, b.Reserve, b.Limits = "", false, 0, nil
	if b.Seen == nil {
		b.Seen = map[string]bool{}
	}
	c, ok := e.credential(b.Auth)
	if !ok {
		return
	}
	for _, event := range e.shadowWarnings(c, b.Model, b.Expected) {
		if b.Seen[event.ID] {
			continue
		}
		b.Seen[event.ID] = true
		e.emit(b, event.Kind, event.Text)
	}
}

func (e *Engine) ShadowWarnings(auth, model string, requests ...adaptive.Request) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.credential(auth)
	if !ok || e.broken != nil {
		return nil
	}
	r := adaptive.Request{Model: model}
	if len(requests) > 0 {
		r = requests[0]
	}
	events := e.shadowWarnings(c, model, r)
	for i := range events {
		events[i].Text = "[CPA quota warning] " + events[i].Text
	}
	return events
}
