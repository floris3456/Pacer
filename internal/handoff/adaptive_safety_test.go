package handoff

import (
	"math"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

func TestAdaptiveDistinctAttemptsAndLateTelemetry(t *testing.T) {
	f := fakeFleetFor(t, "enforce", false)
	r := adaptive.Request{ID: "wire", TraceID: "host-trace", Model: "light", Input: 5000}
	flight, err := f.e.Pick("caller", "claude", "light", []string{"small"}, nil, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.StartAdmission(flight.Admission, "small"); err != nil {
		t.Fatal(err)
	}
	ticket := f.e.state.Adaptive.Tickets[flight.Admission]
	meta := UsageContext{TraceID: r.TraceID, RequestModel: r.Model}
	use := func(id string, output float64, evidence UsageContext) {
		t.Helper()
		if err := f.e.Usage(id, "small", "light", "", 1000, 800, 100, output, evidence); err != nil {
			t.Fatal(err)
		}
	}
	use("execution-1", 100, meta)
	use("execution-1", 100, meta)
	first := ticket.Value
	if first != 705 || len(ticket.Usage) != 1 || ticket.Charge(f.now, 5) != ticket.Estimated {
		t.Fatal(ticket)
	}
	use("execution-2", 200, meta)
	if ticket.Value != first+1105 {
		t.Fatal("distinct execution was lost", ticket)
	}
	f.e.Complete(flight.Admission, true)
	if ticket.Charge(f.now, 5) != ticket.Estimated {
		t.Fatal("released before telemetry settled")
	}
	f.now += 6
	if ticket.Charge(f.now, 5) != 1810 {
		t.Fatal("measured cost did not release unused envelope")
	}
	// A late record restarts the quiet interval; duplicates do not.
	use("execution-3", 300, meta)
	if ticket.Charge(f.now, 5) != ticket.Estimated {
		t.Fatal("late usage released prematurely")
	}
	meta.Failed = true
	use("execution-4", 0, meta)
	f.now += 6
	if ticket.Charge(f.now, 5) < ticket.Estimated+3315 || !ticket.Uncertain {
		t.Fatal("failed internal attempt lost", ticket)
	}
}

func TestAdaptiveAliasesShareDebtAndSampler(t *testing.T) {
	f := fakeFleetFor(t, "enforce", false)
	r := adaptive.Request{ID: "active", Model: "light", Input: 5000}
	flight, err := f.e.Pick("caller", "claude", "light", []string{"small"}, nil, r)
	if err != nil {
		t.Fatal(err)
	}
	f.e.cfg.Credentials = append(f.e.cfg.Credentials, Credential{ID: "alias", Index: "alias-index", Provider: "claude", Account: "small", Alias: "alias"})
	if err = f.e.Identify("small", "same-provider-account"); err != nil {
		t.Fatal(err)
	}
	c, _ := f.e.Credential("alias")
	if c.Account != Digest("claude:same-provider-account") || f.e.state.Adaptive.Tickets[flight.Admission].Account != account(c) {
		t.Fatal("alias split the budget")
	}
	if len(f.e.Readings()) != 6 {
		t.Fatal("identity folding lost or duplicated sampler readings", f.e.Readings())
	}
	if len(f.e.state.Accounts[account(c)].Samples) != 2 {
		t.Fatal("sampler observations were lost")
	}
}

func TestAdaptiveSummaryModelHasSeparateScopedReserve(t *testing.T) {
	f := fakeFleetFor(t, "enforce", false)
	m := f.e.state.Adaptive.Models["claude:small"]
	m.Rates = append(m.Rates, adaptive.Rate{Scope: "heavy", Seconds: 604800, Reset: f.accounts["small"].weekReset, Observed: f.now, Mean: 6000, Low: 4500, Samples: 128})
	if err := f.e.ImportModels(adaptive.Exchange{Protocol: 1, Models: []adaptive.Model{m}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Observe("small", []quota.Window{{Scope: "heavy", Seconds: 604800, Remaining: 2, Reset: f.accounts["small"].weekReset, Observed: f.now, Source: "api", Covered: f.now - 5}}); err != nil {
		t.Fatal(err)
	}
	q, _ := fakeClaim(0)
	r := adaptive.Request{ID: "light-request-heavy-summary", Model: "light", Input: 1000}
	flight, err := f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	if flight.Auth != "medium" {
		t.Fatal("small cannot cover the possible heavy-model summary", flight)
	}
}

func TestAdaptiveFailureAndExternalShockRequireNewCheckpoint(t *testing.T) {
	f := fakeFleetFor(t, "enforce", false)
	q, cap := fakeClaim(0)
	r := adaptive.Request{ID: "first", Model: "light", Input: 1000}
	flight, err := f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	f.execute(t, flight, r, 1000, 800, 100, 100, true)
	q.Generation = flight.Generation
	q.Fresh = false
	// Unexpected external usage is authoritative as soon as it is observed.
	f.accounts["small"].spent = f.accounts["small"].capacity * .98
	f.now += 6
	f.refresh(t)
	snapshot, err := f.e.Snapshot(cap, 0, q.Route)
	if err != nil || snapshot.Intent == "" {
		t.Fatal("shock did not request handoff", snapshot, err)
	}
	q.Intent = snapshot.Intent
	q.Kind = "compaction"
	q.Request = "failed-summary"
	r.ID = q.Request
	r.Kind = q.Kind
	r.Input = 5096
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	if flight.Auth != "medium" {
		t.Fatal("shock left insufficient summary reserve on small")
	}
	f.execute(t, flight, r, 5096, 0, 5096, 50, false)
	if f.e.state.Bindings[flight.Key].SummaryReady {
		t.Fatal("failed summary became ready")
	}
	q.Kind = "primary"
	q.Request = "without-checkpoint"
	r.ID = q.Request
	r.Kind = q.Kind
	if _, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r); err == nil {
		t.Fatal("failed summary authorized handoff")
	}
	// Restoring the percentage without a provider-confirmed cycle reset cannot
	// silently cancel the intent or permit a return to the smaller account.
	f.accounts["small"].spent = 0
	f.now += 6
	f.refresh(t)
	snapshot, _ = f.e.Snapshot(cap, 0, q.Route)
	if snapshot.Intent == "" {
		t.Fatal("unconfirmed recovery canceled the intent")
	}
	q.Kind = "compaction"
	q.Request = "successful-summary"
	r.ID = q.Request
	r.Kind = q.Kind
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	f.execute(t, flight, r, 5096, 0, 5096, 200, true)
	q.Kind = "primary"
	q.Request = "ack"
	q.Checkpoint = "saved-summary"
	r.ID = q.Request
	r.Kind = q.Kind
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil || flight.Auth != "medium" {
		t.Fatal("checkpoint did not hand off", flight, err)
	}
	f.execute(t, flight, r, 1000, 800, 100, 100, true)
	q.Generation = flight.Generation
	q.Intent = ""
	q.Request = "same-cycle-checkpoint"
	q.Checkpoint = "new-checkpoint"
	r.ID = q.Request
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil || flight.Auth != "medium" {
		t.Fatal("returned before confirmed reset", flight, err)
	}
	f.execute(t, flight, r, 1000, 800, 100, 100, true)
	f.now = f.accounts["small"].hourReset + 1
	f.accounts["small"].hourReset = f.now + 18000
	f.accounts["medium"].hourReset = f.now + 18000
	f.accounts["large"].hourReset = f.now + 18000
	f.accounts["small"].spent = 0
	f.refresh(t)
	q.Request = "after-reset-old-context"
	r.ID = q.Request
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil || flight.Auth != "medium" {
		t.Fatal("returned with the old checkpoint", flight, err)
	}
	f.execute(t, flight, r, 1000, 800, 100, 100, true)
	q.Request = "after-reset-new-context"
	q.Checkpoint = "fresh-after-reset"
	r.ID = q.Request
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil || flight.Auth != "small" {
		t.Fatal("fresh checkpoint did not permit recovered smaller credential", flight, err)
	}
}

func TestAdaptiveConfigRejectsNonFiniteMargins(t *testing.T) {
	for _, n := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := adaptive.Config{Margin: n}
		c.Defaults()
		if c.Valid() {
			t.Fatal("invalid margin accepted")
		}
	}
}
