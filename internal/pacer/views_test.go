package pacer

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"testing"
	"time"
)

func TestAccountViewDoesNotHidePartialOrConflictingEvidenceBehindLastWindow(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	remaining := 60.0
	a := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &remaining, Reset: now.Add(time.Hour), Usage: observation.Field{At: now}, ResetEvidence: observation.Field{At: now, Quality: observation.Absolute}}
	b := a
	b.Seconds = 604800
	if accountState("claude", []observation.Window{a}, now) != "partial" {
		t.Fatal("missing weekly budget looked complete")
	}
	if accountState("codex", []observation.Window{a}, now) != "current" {
		t.Fatal("invented mandatory second Codex window")
	}
	a.Conflict = true
	for _, windows := range [][]observation.Window{{a, b}, {b, a}} {
		if accountState("claude", windows, now) != "conflicting" {
			t.Fatal("window iteration order hid conflict")
		}
	}
	a.Conflict = false
	a.Usage.At = now.Add(-time.Hour)
	if accountState("claude", []observation.Window{a, b}, now) != "stale" {
		t.Fatal("fresh sibling hid stale balance")
	}
}
