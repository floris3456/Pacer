package handoff

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

type fakeQuota struct {
	capacity             float64
	spent                float64
	hourReset, weekReset int64
	weekly               bool
}
type fakeFleet struct {
	e        *Engine
	now      int64
	accounts map[string]*fakeQuota
	cfg      Config
}

func fakeFleetFor(t *testing.T, mode string, weekly bool) *fakeFleet {
	t.Helper()
	f := &fakeFleet{now: 1800000000, accounts: map[string]*fakeQuota{}}
	f.cfg = Config{StatePath: filepath.Join(t.TempDir(), "budget.sqlite"), Adaptive: adaptive.Config{Mode: mode}, Credentials: []Credential{}}
	for i, size := range []string{"small", "medium", "large"} {
		f.accounts[size] = &fakeQuota{capacity: float64([]int{600000, 2400000, 9600000}[i]), hourReset: f.now + 18000, weekReset: f.now + 604800, weekly: weekly}
		f.cfg.Credentials = append(f.cfg.Credentials, Credential{ID: size, Index: size, Provider: "claude", Account: size, Alias: size, Rank: i})
	}
	var err error
	f.e, err = Open(f.cfg, func() time.Time { return time.Unix(f.now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.e.Close() })
	f.refresh(t)
	return f
}
func (f *fakeFleet) refresh(t *testing.T) {
	t.Helper()
	x := adaptive.Exchange{Protocol: 1, Generated: f.now}
	for id, a := range f.accounts {
		rate := a.capacity / 100
		m := adaptive.Model{Account: id, AuthIndices: []string{id}, Version: fmt.Sprint(f.now), Rule: "api-equivalent-v1", Generated: f.now, Expires: f.now + 300, Through: f.now, ObservationsThrough: f.now, Profiles: []adaptive.Profile{
			{Model: "light", Samples: 128, Weights: adaptive.Weights{Input: 1, Read: .1, Write: 1.25, Output: 4}, InputMean: 2000, OutputMean: 300, OutputHigh: 1200, ReadFraction: .8, WriteFraction: .1},
			{Model: "heavy", Samples: 128, Weights: adaptive.Weights{Input: 2, Read: .2, Write: 2.5, Output: 8}, InputMean: 2000, OutputMean: 300, OutputHigh: 1200, ReadFraction: .8, WriteFraction: .1},
		}}
		hr, wr := rate, rate*10
		if a.weekly {
			hr, wr = rate*10, rate
		}
		m.Rates = []adaptive.Rate{{Scope: "all", Seconds: 18000, Reset: a.hourReset, Observed: f.now, Mean: hr, Low: hr * .75, Samples: 128}, {Scope: "all", Seconds: 604800, Reset: a.weekReset, Observed: f.now, Mean: wr, Low: wr * .75, Samples: 128}}
		x.Models = append(x.Models, m)
		hour, week := 100-a.spent/hr, 100-a.spent/wr
		if hour < 0 || week < 0 {
			t.Fatalf("fake credential exhausted: %s hour=%.3f week=%.3f", id, hour, week)
		}
		if err := f.e.Observe(id, []quota.Window{
			{Scope: "all", Seconds: 18000, Remaining: hour, Reset: a.hourReset, Observed: f.now, Source: "api", Covered: f.now - 5},
			{Scope: "all", Seconds: 604800, Remaining: week, Reset: a.weekReset, Observed: f.now, Source: "api", Covered: f.now - 5},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.e.ImportModels(x); err != nil {
		t.Fatal(err)
	}
}
func fakeClaim(i int) (*Claim, string) {
	cap := fmt.Sprintf("dummy-capability-session-%d-012345678901234567890", i)
	return &Claim{Route: "claude/light", Protocol: 1, Installation: "fixture", Session: fmt.Sprint(i), Request: fmt.Sprint(i), Kind: "primary", Capability: Digest(cap), Auto: true, Fresh: true, Portable: true}, cap
}
func (f *fakeFleet) execute(t *testing.T, flight Flight, r adaptive.Request, input, read, write, output float64, success bool) {
	t.Helper()
	if err := f.e.StartAdmission(flight.Admission, flight.Auth); err != nil {
		t.Fatal(err)
	}
	// The fake provider bills independently of the estimator, including real
	// cache categories and a different summary model. No real account is read.
	multiplier := 1.
	if r.Model == "heavy" {
		multiplier = 2
	}
	cost := multiplier * ((input - read - write) + read*.1 + write*1.25 + output*4)
	f.accounts[flight.Auth].spent += cost
	if err := f.e.Usage(r.ID, flight.Auth, r.Model, "", input, read, write, output); err != nil {
		t.Fatal(err)
	}
	// Duplicate fanout must be idempotent.
	if err := f.e.Usage(r.ID, flight.Auth, r.Model, "", input, read, write, output); err != nil {
		t.Fatal(err)
	}
	f.e.Complete(flight.Admission, success)
}

func TestAdaptiveConcurrentFakeCredentials(t *testing.T) {
	type metric struct {
		Capacity    float64 `json:"small_capacity"`
		Chats       int     `json:"chats"`
		Weekly      bool    `json:"weekly"`
		Seed        int     `json:"seed"`
		Requests    int     `json:"requests"`
		Compactions int     `json:"compactions"`
		Remaining   float64 `json:"small_remaining_percent"`
	}
	var results []metric
	for _, scale := range []float64{1, 5} {
		for _, weekly := range []bool{false, true} {
			for _, n := range []int{1, 2, 3, 5, 8} {
				for seed := 0; seed < 3; seed++ {
					t.Run(fmt.Sprintf("scale_%.0f_chats_%d_weekly_%v_seed_%d", scale, n, weekly, seed), func(t *testing.T) {
						f := fakeFleetFor(t, "enforce", weekly)
						for _, a := range f.accounts {
							a.capacity *= scale
						}
						f.refresh(t)
						claims := make([]*Claim, n)
						caps := make([]string, n)
						done := make([]bool, n)
						contexts := make([]float64, n)
						for i := range claims {
							claims[i], caps[i] = fakeClaim(i)
							contexts[i] = float64(900 + i*250 + seed*100)
						}
						metric := metric{Chats: n, Weekly: weekly, Seed: seed, Capacity: f.accounts["small"].capacity}
						for wave := 0; wave < 800; wave++ {
							remaining := 0
							requests := make([]adaptive.Request, n)
							flights := make([]Flight, n)
							errs := make([]error, n)
							for i, q := range claims {
								if done[i] {
									continue
								}
								remaining++
								snap, err := f.e.Snapshot(caps[i], 0, q.Route)
								if err == nil {
									q.Generation = snap.Generation
									q.Intent = snap.Intent
									q.Fresh = false
								}
								q.Kind = "primary"
								input := contexts[i]
								model := "light"
								if (i+seed)%3 == 0 {
									model = "heavy"
								}
								if q.Intent != "" && q.Checkpoint == "" {
									q.Kind = "compaction"
									model = "heavy"
									input += 4096
								}
								q.Request = fmt.Sprintf("%d/%d/%s", i, wave, q.Kind)
								requests[i] = adaptive.Request{ID: q.Request, Model: model, Input: input, MaxOutput: 4096, Kind: q.Kind}
							}
							if remaining == 0 {
								break
							}
							var wg sync.WaitGroup
							for i, q := range claims {
								if done[i] {
									continue
								}
								wg.Add(1)
								go func(i int, q *Claim) {
									defer wg.Done()
									flights[i], errs[i] = f.e.Pick("caller", "claude", requests[i].Model, []string{"small", "medium", "large"}, q, requests[i])
								}(i, q)
							}
							wg.Wait()
							for i, q := range claims {
								if done[i] {
									continue
								}
								if errs[i] != nil {
									if r, ok := errs[i].(*Refusal); ok && (r.Code == "quota_checkpoint_required" || r.Code == "quota_generation_changed" || r.Code == "quota_budget_wait") {
										continue
									}
									t.Fatalf("wave %d chat %d: %v", wave, i, errs[i])
								}
								r := requests[i]
								if flights[i].Auth != "small" {
									if q.Checkpoint == "" && wave > 0 {
										t.Fatal("switched without saved checkpoint")
									}
									done[i] = true
								}
								read, write, output := r.Input*.8, r.Input*.1, float64(150+(wave*73+i*101+seed*19)%1000)
								if wave%11 == 7 {
									read = 0
									write = r.Input * .8
								} // simultaneous cold caches
								if q.Kind == "compaction" {
									read = 0
									write = r.Input
									output = 2048
									metric.Compactions++
									q.Checkpoint = fmt.Sprintf("saved/%d/%d", i, wave)
								} else {
									contexts[i] += float64(30 + (wave+i)%150)
									metric.Requests++
								}
								f.execute(t, flights[i], r, r.Input, read, write, output, true)
								q.Generation = flights[i].Generation
							}
							// Independent provider readings trail completion; stale and duplicate
							// callback evidence cannot free an unfinished reservation.
							f.now += 6
							f.refresh(t)
						}
						for i, finished := range done {
							if !finished {
								t.Fatalf("chat %d did not hand off", i)
							}
						}
						metric.Remaining = 100 - f.accounts["small"].spent/f.accounts["small"].capacity*100
						if metric.Remaining < 0 || metric.Remaining > 35 {
							t.Fatalf("excessive loss or overdraw: %+v", metric)
						}
						results = append(results, metric)
						t.Logf("%d chats weekly=%v: %.2f%% remains, %d requests, %d summaries", n, weekly, metric.Remaining, metric.Requests, metric.Compactions)
					})
				}
			}
		}
	}
	if path := os.Getenv("QUOTA_SIMULATION_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdaptiveReservationsRestartAliasesAndLateUsage(t *testing.T) {
	f := fakeFleetFor(t, "enforce", false)
	q, cap := fakeClaim(0)
	r := adaptive.Request{ID: "running", Model: "light", Input: 4000, Kind: "primary"}
	flight, err := f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.StartAdmission(flight.Admission, flight.Auth); err != nil {
		t.Fatal(err)
	}
	if err = f.e.StartAdmission(flight.Admission, flight.Auth); err != nil {
		t.Fatal("duplicate host after-auth hook was not idempotent", err)
	}
	if _, err = f.e.Pick("caller", "claude", "light", []string{"small", "medium"}, q, r); err == nil {
		t.Fatal("unreserved retry admitted")
	}
	projection := func() adaptive.Projection {
		for _, p := range f.e.Projections() {
			if p.Account == "claude:small" && p.Seconds == 18000 {
				return p
			}
		}
		t.Fatal("missing small projection")
		return adaptive.Projection{}
	}
	before := projection()
	f.now += 140
	f.refresh(t)
	after := projection()
	if after.Unfinished <= 0 || after.Compaction <= 0 || after.Active != 1 {
		t.Fatalf("active request expired with heartbeat: before=%+v after=%+v", before, after)
	}
	if _, err = f.e.Snapshot(cap, 0, q.Route); err != nil {
		t.Fatal(err)
	}
	if err = f.e.Close(); err != nil {
		t.Fatal(err)
	}
	f.e, err = Open(f.cfg, func() time.Time { return time.Unix(f.now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	r.ID = "after-restart"
	q.Request = "after-restart"
	if _, err = f.e.Pick("caller", "claude", "light", []string{"small"}, nil, r); err == nil {
		t.Fatal("restart released unfinished spend")
	}
	// Confirmed reset clears the old-cycle reservation, not an arbitrary timeout.
	f.now = f.accounts["small"].weekReset + 10
	for _, a := range f.accounts {
		a.spent = 0
		a.hourReset = f.now + 18000
		a.weekReset = f.now + 604800
	}
	f.refresh(t)
	if _, err = f.e.Pick("caller", "claude", "light", []string{"small"}, nil, r); err != nil {
		t.Fatal(err)
	}
}

func TestAdaptiveWindowDebtAndSummaryAreNotDoubleCharged(t *testing.T) {
	f := fakeFleetFor(t, "enforce", false)
	q, _ := fakeClaim(0)
	r := adaptive.Request{ID: "one", Model: "light", Input: 1000, Kind: "primary"}
	flight, err := f.e.Pick("caller", "claude", "light", []string{"small"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	f.execute(t, flight, r, 1000, 800, 100, 200, true)
	find := func() adaptive.Projection {
		for _, p := range f.e.Projections() {
			if p.Account == "claude:small" && p.Seconds == 18000 {
				return p
			}
		}
		t.Fatal("projection absent")
		return adaptive.Projection{}
	}
	p := find()
	if p.Unfinished != 0 || p.Unobserved <= 0 {
		t.Fatal(p)
	}
	f.now += 6
	f.refresh(t)
	p = find()
	if math.Abs(p.Unobserved) > 1e-9 {
		t.Fatal("covered work charged twice", p)
	}
	q.Kind = "compaction"
	q.Request = "sum"
	q.Generation = flight.Generation
	r.Kind = "compaction"
	r.ID = "sum"
	r.Input = 5096
	flight, err = f.e.Pick("caller", "claude", "light", []string{"small"}, q, r)
	if err != nil {
		t.Fatal(err)
	}
	p = find()
	if p.Compaction != 0 || p.Unfinished <= 0 {
		t.Fatal("summary plus reservation double charged", p)
	}
	f.execute(t, flight, r, r.Input, 0, r.Input, 2048, true)
	p = find()
	if p.Compaction != 0 {
		t.Fatal("saved summary still reserved", p)
	}
}
