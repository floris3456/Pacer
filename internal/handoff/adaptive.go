package handoff

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

func (e *Engine) AdaptiveConfig() adaptive.Config { return e.cfg.Adaptive }
func (e *Engine) Readings() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, c := range e.cfg.Credentials {
		key := account(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, w := range e.state.Accounts[key].Samples {
			if w.NotApplicable || w.Reset <= w.Observed || e.now().Unix()-w.Observed > 90 {
				continue
			}
			out = append(out, map[string]any{"auth_index": c.Index, "provider": c.Provider, "scope": w.Scope, "seconds": w.Seconds, "remaining": w.Remaining, "reset": w.Reset, "observed": w.Observed, "covered_through": w.Covered})
		}
	}
	return out
}
func (e *Engine) NearBoundary(auth string) bool {
	for _, p := range e.Projections() {
		if p.AuthIndex != "" && p.RequestsLow <= int(e.cfg.Adaptive.Lead) && p.Active > 0 {
			if c, ok := e.Credential(auth); ok && c.Index == p.AuthIndex {
				return true
			}
		}
	}
	return false
}
func (e *Engine) ImportModels(x adaptive.Exchange) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x.Protocol != adaptive.Protocol || len(x.Models) > 256 {
		return fmt.Errorf("invalid adaptive exchange")
	}
	for _, m := range x.Models {
		if !m.Valid(e.now().Unix()) {
			return fmt.Errorf("invalid or expired adaptive model")
		}
	}
	for _, c := range e.cfg.Credentials {
		for _, m := range x.Models {
			for _, index := range m.AuthIndices {
				if index == c.Index {
					old := e.state.Adaptive.Models[account(c)]
					if m.Generated >= old.Generated && m.Through >= old.Through && m.ObservationsThrough >= old.ObservationsThrough {
						e.state.Adaptive.Models[account(c)] = m
					}
				}
			}
		}
	}
	e.reconcileAll()
	return e.save()
}
func (e *Engine) windowBudget(w quota.Window) adaptive.Window {
	return adaptive.Window{Key: w.Key(), Scope: w.Scope, Seconds: w.Seconds, Remaining: w.Remaining, Reset: w.Reset, Cycle: w.Cycle, Covered: w.Covered}
}
func (e *Engine) trimAdaptive(account string) {
	if !e.cfg.Adaptive.Enabled() {
		return
	}
	windows := []adaptive.Window{}
	for _, w := range e.state.Accounts[account].Windows {
		windows = append(windows, e.windowBudget(w))
	}
	e.state.Adaptive.Trim(account, windows)
}
func (e *Engine) activeSessions(account string) []adaptive.Session {
	var out []adaptive.Session
	for _, b := range e.state.Bindings {
		if b.Account != account {
			continue
		}
		if b.Expected.ID == "" {
			continue
		} // A standalone summary has no future ordinary context.
		active := e.now().Unix()-b.Last <= e.cfg.Adaptive.LeaseSeconds
		summarizing := b.SummaryReady
		for _, t := range e.state.Adaptive.Tickets {
			if t.Session == b.Key && t.Completed == 0 {
				active = true
				summarizing = summarizing || t.Request.Kind == "compaction"
			}
		}
		if !active {
			continue
		}
		r := b.Expected
		if e.pendingKey == b.Key && e.pending != nil && e.pending.Kind != "compaction" {
			r = *e.pending
		}
		out = append(out, adaptive.Session{ID: b.Key, Request: r, Auto: b.Auto, Summarizing: summarizing})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (e *Engine) adaptiveWindow(c Credential, model string, w quota.Window) bool {
	if !e.cfg.Adaptive.Enforce() {
		return false
	}
	m := e.state.Adaptive.Models[account(c)]
	_, ok := m.Rate(w.Scope, w.Seconds, w.Reset, e.now().Unix())
	if !ok {
		return false
	}
	r := adaptive.Request{Model: model, Tier: "", Input: 1}
	if e.pending != nil {
		r = *e.pending
	}
	_, ok = m.Profile(r.Model, r.Tier)
	return ok && !r.Opaque
}
func (e *Engine) canReturn(c Credential) bool {
	b := e.state.Bindings[e.pendingKey]
	if b == nil {
		return true
	}
	for key, cycle := range b.Departed[account(c)] {
		if e.state.Accounts[account(c)].Windows[key].Cycle == cycle {
			return false
		}
	}
	return true
}
func (e *Engine) adaptiveEligible(c Credential, model string) bool {
	if !e.cfg.Adaptive.Enforce() {
		return true
	}
	if !e.canReturn(c) {
		return false
	}
	if e.pending == nil {
		return true
	}
	r := *e.pending
	auto := e.pendingKey != "" && r.Kind != "compaction"
	if b := e.state.Bindings[e.pendingKey]; b != nil {
		auto = auto && b.Auto
	}
	sessions := e.activeSessions(account(c))
	if b := e.state.Bindings[e.pendingKey]; e.pendingKey != "" && (b == nil || b.Account != account(c)) && r.Kind != "compaction" {
		auto := true
		if b != nil {
			auto = b.Auto
		}
		sessions = append(sessions, adaptive.Session{ID: e.pendingKey, Request: r, Auto: auto})
	}
	if r.Kind == "compaction" {
		for i := range sessions {
			if sessions[i].ID == e.pendingKey {
				sessions[i].Summarizing = true
			}
		}
	}
	for _, w := range e.budgetWindows(account(c), model, auto) {
		if w.NotApplicable {
			continue
		}
		if e.now().Unix()-w.Observed > e.cfg.MaxAge || w.Reset <= e.now().Unix() {
			return false
		}
		p := e.state.Adaptive.Project(account(c), e.windowBudget(w), sessions, e.cfg.Adaptive, e.now().Unix())
		cost, _, _ := e.state.Adaptive.Cost(account(c), e.windowBudget(w), r, e.now().Unix())
		if !adaptive.Applies(model, w.Scope) {
			cost = 0
		}
		if p.Orphan || p.Headroom < cost {
			return false
		}
	}
	return true
}

// Reserve known scoped windows for a possible different summary model as well
// as for the model serving the current request.
func (e *Engine) budgetWindows(account, model string, auto bool) []quota.Window {
	var out []quota.Window
	for _, w := range e.state.Accounts[account].Windows {
		if adaptive.Applies(model, w.Scope) || (auto && e.state.Adaptive.Models[account].SummaryApplies(w.Scope)) {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func (e *Engine) adaptiveWaitable(b *Binding) bool {
	if !e.cfg.Adaptive.Enforce() || e.pending == nil || b.Intent != "" {
		return false
	}
	waiting := false
	for _, w := range e.budgetWindows(b.Account, b.Model, b.Auto) {
		if w.NotApplicable {
			continue
		}
		p := e.state.Adaptive.Project(b.Account, e.windowBudget(w), e.activeSessions(b.Account), e.cfg.Adaptive, e.now().Unix())
		high, _, _ := e.state.Adaptive.Cost(b.Account, e.windowBudget(w), *e.pending, e.now().Unix())
		if !adaptive.Applies(b.Model, w.Scope) {
			high = 0
		}
		if p.Orphan || e.now().Unix()-w.Observed > e.cfg.MaxAge || w.Reset <= e.now().Unix() {
			return false
		}
		if p.Headroom < high {
			if p.Headroom+e.state.Adaptive.PendingSavings(b.Account, e.windowBudget(w), e.now().Unix()) < high {
				return false
			}
			waiting = true
		}
	}
	return waiting
}

// Pick keeps the selection and spend reservation under the same lock. Optional
// requests preserve the old engine API for fixed-threshold deployments.
func (e *Engine) Pick(caller, provider, model string, candidates []string, claim *Claim, requests ...adaptive.Request) (Flight, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() { e.pending = nil; e.pendingKey = "" }()
	if e.cfg.Adaptive.Enabled() {
		if len(requests) == 0 || requests[0].ID == "" {
			if e.cfg.Adaptive.Enforce() {
				return Flight{}, refuse("quota_request_identity_missing", "Adaptive admission requires a host request identity")
			}
		} else {
			r := requests[0]
			r.Model = model
			if claim != nil {
				r.Kind = claim.Kind
				e.pendingKey = Digest(caller + "\x00" + claim.Installation + "\x00" + claim.Session + "\x00" + provider + "\x00" + claim.Route)
			}
			e.pending = &r
			if e.cfg.Adaptive.Enforce() {
				for _, ticket := range e.state.Adaptive.Tickets {
					if ticket.WireID == r.ID && ticket.Executing {
						return Flight{}, refuse("quota_retry_requires_new_request", "Retry requires a new budgeted request")
					}
				}
			}
		}
	}
	var f Flight
	var err error
	if e.cfg.Adaptive.Shadow() {
		// In shadow mode the bridge supplies the credential already selected by CPA.
		f, err = e.observeSelection(caller, provider, model, candidates, claim)
	} else {
		f, err = e.pick(caller, provider, model, candidates, claim)
	}
	if err != nil || e.pending == nil {
		return f, err
	}
	if f.Admission == "" {
		f.Admission = "ordinary:" + e.pending.ID
	}
	if _, exists := e.state.Adaptive.Tickets[f.Admission]; exists {
		return f, nil
	}
	m := e.state.Adaptive.Models[f.Account]
	cost := m.Estimate(*e.pending, false, e.now().Unix())
	t := &adaptive.Ticket{ID: f.Admission, WireID: e.pending.ID, Account: f.Account, Session: f.Key, Request: *e.pending, Started: e.now().Unix(), Value: cost.High, Estimated: cost.High, Cycles: map[string]int64{}, Fallback: map[string]float64{}}
	for _, w := range e.windows(f.Account, model) {
		if w.NotApplicable {
			continue
		}
		t.Cycles[w.Key()] = w.Cycle
		points, _, _ := e.state.Adaptive.Cost(f.Account, e.windowBudget(w), *e.pending, e.now().Unix())
		t.Fallback[w.Key()] = points
	}
	e.state.Adaptive.Tickets[f.Admission] = t
	e.reconcileAll()
	if err = e.save(); err != nil {
		return Flight{}, err
	}
	return f, nil
}
func (e *Engine) completeAdaptive(id string, success bool) {
	if !e.cfg.Adaptive.Enabled() {
		return
	}
	if t := e.state.Adaptive.Tickets[id]; t != nil {
		t.Completed = e.now().Unix()
		t.Succeeded = success
		if t.Request.Kind == "compaction" && success {
			if b := e.state.Bindings[t.Session]; b != nil {
				b.SummaryReady = true
			}
		}
	}
	e.reconcileAll()
	_ = e.save()
}

func (e *Engine) StartAdmission(id, auth string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Adaptive.Enforce() {
		return nil
	}
	t := e.state.Adaptive.Tickets[id]
	c, ok := e.credential(auth)
	if t == nil || !ok || t.Account != account(c) {
		return refuse("quota_reservation_missing", "Request has no matching spend reservation")
	}
	if t.Executing {
		return nil // CPA and its executor can both invoke the after-auth hook.
	}
	t.Executing = true
	return e.save()
}

type UsageContext struct {
	TraceID, RequestModel string
	Failed                bool
}

func (e *Engine) Usage(id, auth, model, tier string, input, read, write, output float64, evidence ...UsageContext) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Adaptive.Enabled() {
		return nil
	}
	if id == "" {
		return nil
	} // Unidentifiable telemetry cannot safely release a reservation.
	c, ok := e.credential(auth)
	if !ok {
		return nil
	}
	var matched *adaptive.Ticket
	meta := UsageContext{RequestModel: model}
	if len(evidence) > 0 {
		meta = evidence[0]
	}
	for _, t := range e.state.Adaptive.Tickets {
		if t.Account != account(c) {
			continue
		}
		traceMatch := meta.TraceID != "" && t.Request.TraceID == meta.TraceID && t.Request.Model == meta.RequestModel
		if t.WireID != id && !traceMatch {
			continue
		}
		if matched != nil {
			return nil
		} // Ambiguity retains both reservations.
		matched = t
	}
	if matched != nil {
		if matched.Usage == nil {
			matched.Usage = map[string]float64{}
		}
		if _, duplicate := matched.Usage[id]; duplicate {
			return nil
		}
		v, ok := e.state.Adaptive.Models[matched.Account].Actual(model, tier, input, read, write, output)
		if !ok {
			v = 0
		}
		if ok && v > 0 && !meta.Failed {
			matched.Actual = true
		} else {
			// A missing/error record is one potentially billed attempt. Keep
			// its envelope in addition to other distinct, measured attempts.
			v = math.Max(v, matched.Estimated)
			matched.Uncertain = true
		}
		matched.Usage[id] = v
		matched.Value = 0
		for _, cost := range matched.Usage {
			matched.Value += cost
		}
		matched.UsageAt = e.now().Unix()
	}
	e.reconcileAll()
	return e.save()
}
func (e *Engine) rememberDeparture(b *Binding) {
	if !e.cfg.Adaptive.Enabled() || b.Intent == "" {
		return
	}
	if b.Departed == nil {
		b.Departed = map[string]map[string]int64{}
	}
	if len(b.Limits) > 0 {
		b.Departed[b.Account] = b.Limits
		b.Limits = nil
	}
}
func (e *Engine) reconcileAll() {
	keys := []string{}
	for key := range e.state.Bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		e.reconcile(e.state.Bindings[key], false)
	}
}
func (e *Engine) reconcileAdaptive(b *Binding) bool {
	if !e.cfg.Adaptive.Enforce() || b.Expected.ID == "" {
		return false
	}
	if b.AdaptiveSeen == nil {
		b.AdaptiveSeen = map[string]bool{}
	}
	if b.Limits == nil {
		b.Limits = map[string]int64{}
	}
	if e.now().Unix()-b.Last > e.cfg.Adaptive.LeaseSeconds {
		return true
	}
	sessions := e.activeSessions(b.Account)
	for _, w := range e.budgetWindows(b.Account, b.Model, b.Auto) {
		if w.NotApplicable {
			continue
		}
		p := e.state.Adaptive.Project(b.Account, e.windowBudget(w), sessions, e.cfg.Adaptive, e.now().Unix())
		high, _, known := e.state.Adaptive.Cost(b.Account, e.windowBudget(w), b.Expected, e.now().Unix())
		if !adaptive.Applies(b.Model, w.Scope) {
			high = 0
		}
		forecastHeadroom := p.Headroom + e.state.Adaptive.PendingSavings(b.Account, e.windowBudget(w), e.now().Unix())
		urgent := p.Orphan || forecastHeadroom < high || w.Remaining <= 0 || (!known && w.Held)
		if !urgent && p.Headroom > high*e.cfg.Adaptive.Lead {
			continue
		}
		kind := "warning"
		if urgent {
			kind = "handoff"
		}
		id := fmt.Sprintf("%s:%s:%d:%s", b.Account, w.Key(), w.Cycle, kind)
		if urgent {
			b.Limits[w.Key()] = w.Cycle
			if b.Intent == "" {
				b.Intent = Digest(b.Key + id + fmt.Sprint(b.Generation))
				b.Reserve = 0
			}
		}
		if b.AdaptiveSeen[id] {
			continue
		}
		b.AdaptiveSeen[id] = true
		text := e.adaptiveText(b.Alias, w, p, urgent)
		if urgent && b.Auto {
			text += " This chat will compact at its next safe boundary, then switch after its saved summary is acknowledged."
		}
		if urgent && !b.Auto {
			text += " Automatic compaction is off; the next portable request may switch credentials."
		}
		e.emit(b, kind, text)
	}
	// Predictive intent is latched until a checkpoint or actual limiting-cycle
	// reset; a cost-model update or another chat leaving cannot cancel it.
	if b.Intent != "" && len(b.Limits) > 0 {
		reset := true
		for key, cycle := range b.Limits {
			if e.state.Accounts[b.Account].Windows[key].Cycle == cycle {
				reset = false
			}
		}
		if reset {
			b.Intent = ""
			b.Reserve = 0
			b.Limits = nil
			e.emit(b, "recovered", "The limiting quota window has reset. This chat will recheck its existing credential.")
		}
	}
	return true
}
func (e *Engine) adaptiveText(alias string, w quota.Window, p adaptive.Projection, urgent bool) string {
	label := "five-hour"
	if w.Seconds == 604800 {
		label = "seven-day"
	}
	text := fmt.Sprintf("Credential %s has %.1f%% %s quota left; resets %s.", alias, w.Remaining, label, time.Unix(w.Reset, 0).UTC().Format(time.RFC3339))
	if urgent {
		return text + " Compact or start a new chat now to preserve enough capacity for the handoff."
	}
	if p.Active == 0 {
		return text + " To save usage, compact or start a new chat when the next reminder arrives."
	}
	return text + fmt.Sprintf(" Estimated %d–%d more model requests across %d active chats before compaction is needed. To save usage, compact or start a new chat when the next reminder arrives.", p.RequestsLow, p.RequestsHigh, p.Active)
}
func (e *Engine) Projections() []adaptive.Projection {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []adaptive.Projection
	seen := map[string]bool{}
	for _, c := range e.cfg.Credentials {
		key := account(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, w := range e.state.Accounts[key].Windows {
			if w.NotApplicable {
				continue
			}
			p := e.state.Adaptive.Project(key, e.windowBudget(w), e.activeSessions(key), e.cfg.Adaptive, e.now().Unix())
			p.AuthIndex = c.Index
			if e.now().Unix()-w.Observed > e.cfg.MaxAge || w.Reset <= e.now().Unix() {
				p.Quality = "stale"
				p.RequestsLow = 0
				p.RequestsHigh = 0
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account+out[i].Window < out[j].Account+out[j].Window })
	return out
}
func (e *Engine) AdaptiveLegacy(auth, model string, requests ...adaptive.Request) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Adaptive.Enforce() {
		return nil
	}
	c, ok := e.credential(auth)
	if !ok {
		return nil
	}
	var out []Event
	for _, w := range e.windows(account(c), model) {
		if w.NotApplicable || e.now().Unix()-w.Observed > e.cfg.MaxAge || w.Reset <= e.now().Unix() {
			continue
		}
		p := e.state.Adaptive.Project(account(c), e.windowBudget(w), e.activeSessions(account(c)), e.cfg.Adaptive, e.now().Unix())
		r := adaptive.Request{Model: model, Input: 1}
		if profile, ok := e.state.Adaptive.Models[account(c)].Profile(model, ""); ok {
			r.Input = profile.InputMean
		}
		if len(requests) > 0 {
			r = requests[0]
			r.Model = model
		}
		high, _, known := e.state.Adaptive.Cost(account(c), e.windowBudget(w), r, e.now().Unix())
		urgent := p.Headroom < high || (!known && w.Held)
		if !urgent && p.Headroom > math.Max(high, 1)*e.cfg.Adaptive.Lead {
			continue
		}
		kind := "warning"
		if urgent {
			kind = "handoff"
		}
		text := e.adaptiveText(c.Alias, w, p, urgent)
		out = append(out, Event{ID: fmt.Sprintf("adaptive:%s:%s:%d:%s", account(c), w.Key(), w.Cycle, kind), Kind: kind, Text: "[CPA quota warning] " + text})
	}
	return out
}
