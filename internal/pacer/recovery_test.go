package pacer

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"testing"
	"time"
)

func TestRecoveryRequiresAllApplicableNewCycleEvidenceAfterFailure(t *testing.T) {
	now := time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
	failure := now.Add(-time.Minute)
	restriction := cooldown{Scope: "model", Model: "claude-opus-5-5", Classification: "quota", Clearable: true, Revision: "opaque-version", FailureAt: failure, Required: []string{"all/18000", "all/604800"}}
	left := 75.0
	base := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &left, Reset: now.Add(time.Hour), Cycle: "new", CycleAt: now, Usage: observation.Field{At: now, Source: observation.HeaderSource, Quality: observation.Measured}, ResetEvidence: observation.Field{At: now, Source: observation.HeaderSource, Quality: observation.Absolute}}
	long := base
	long.Seconds = 604800
	if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{base, long}, now); !ok {
		t.Fatal("complete new-cycle evidence refused")
	}
	for _, change := range []func(*observation.Window){
		func(w *observation.Window) { w.Remaining = nil }, func(w *observation.Window) { zero := 0.0; w.Remaining = &zero }, func(w *observation.Window) { w.Usage.At = failure }, func(w *observation.Window) { w.ResetEvidence.At = failure }, func(w *observation.Window) { w.Conflict = true }, func(w *observation.Window) { w.Ambiguous = true }, func(w *observation.Window) { w.Reset = now.Add(-time.Second) }, func(w *observation.Window) { w.ResetEvidence.Quality = observation.Estimated }, func(w *observation.Window) { w.Dormant = true },
	} {
		changed := long
		change(&changed)
		if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{base, changed}, now); ok {
			t.Fatal("insufficient long window cleared cooldown", changed)
		}
	}
	if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{base}, now); ok {
		t.Fatal("missing weekly evidence recovered")
	}
	base.CycleAt = failure
	long.CycleAt = failure
	if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{base, long}, now); ok {
		t.Fatal("old cycle presented as reset")
	}
	base.CycleAt = now
	long.CycleAt = now
	restriction.Classification = "non_quota"
	if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{base, long}, now); ok {
		t.Fatal("nonquota reset")
	}
}

// The 5-hour window hit its limit, then reset without being used: the meter read
// after the failure shows it dormant (100 % left, no reset).
func TestRecoveryAcceptsADormantWindowReadAfterTheFailure(t *testing.T) {
	now := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	failure := now.Add(-5 * time.Hour)
	restriction := cooldown{Scope: "credential", Classification: "quota", Clearable: true, Revision: "opaque-version", FailureAt: failure, Required: []string{"all/18000"}}
	full, weekly := 100.0, 60.0
	short := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &full, Dormant: true, Usage: observation.Field{At: now, Source: observation.APISource, Quality: observation.Measured}, StatusEvidence: observation.Field{At: now, Source: observation.APISource, Quality: observation.Measured, Raw: "null"}}
	long := observation.Window{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &weekly, Reset: now.Add(48 * time.Hour), Cycle: "weekly", CycleAt: failure.Add(-time.Hour), Usage: observation.Field{At: now, Source: observation.APISource, Quality: observation.Measured}, ResetEvidence: observation.Field{At: now, Source: observation.APISource, Quality: observation.Absolute}}
	evidence, ok := evidenceForRecovery("claude", restriction, []observation.Window{short, long}, now)
	if !ok || evidence.Source != "quota_endpoint" || !evidence.At.Equal(now) {
		t.Fatal("dormant window after the failure refused", evidence)
	}
	for name, change := range map[string]func(*observation.Window){
		"read before the failure": func(w *observation.Window) { w.StatusEvidence.At = failure },
		"stale": func(w *observation.Window) {
			w.StatusEvidence.At = now.Add(-2 * time.Hour)
			w.Usage.At = w.StatusEvidence.At
		},
		"from headers": func(w *observation.Window) { w.StatusEvidence.Source = observation.HeaderSource },
	} {
		changed := short
		change(&changed)
		if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{changed, long}, now); ok {
			t.Fatal(name, "cleared cooldown")
		}
	}
	exhausted := 0.0
	long.Remaining = &exhausted
	if _, ok := evidenceForRecovery("claude", restriction, []observation.Window{short, long}, now); ok {
		t.Fatal("exhausted weekly window cleared cooldown")
	}
}

func TestDormantReadingClosesTheMeasuredCycle(t *testing.T) {
	p := startPlugin(t, t.TempDir()+"/state.sqlite")
	defer p.Close()
	p.mu.Lock()
	c := p.creds["a"]
	p.mu.Unlock()
	if c.Account == "" {
		t.Fatal("fixture credential missing")
	}
	at := p.now().UTC().Add(-time.Hour)
	left := 40.0
	running := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &left, Reset: at.Add(30 * time.Minute), Usage: observation.Field{At: at, Source: observation.APISource, Quality: observation.Measured}, ResetEvidence: observation.Field{At: at, Source: observation.APISource, Quality: observation.Absolute}}
	p.storeObservations(c, []observation.Window{running})
	read := at.Add(45 * time.Minute)
	full := 100.0
	dormant := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &full, Dormant: true, Usage: observation.Field{At: read, Source: observation.APISource, Quality: observation.Measured}, StatusEvidence: observation.Field{At: read, Source: observation.APISource, Quality: observation.Measured, Raw: "null"}}
	p.storeObservations(c, []observation.Window{dormant})
	var closures int
	if err := p.db.QueryRow(`SELECT count(*) FROM measurement_closures`).Scan(&closures); err != nil || closures != 1 {
		t.Fatal("cycle replaced by a dormant reading not closed", closures, err)
	}
	if p.measurementErrors != 0 {
		t.Fatal("measurement errors", p.measurementErrors)
	}
}
