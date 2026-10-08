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
