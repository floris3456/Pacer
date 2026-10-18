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

// An unstarted 5-hour window does not make an account with a running 7-day
// window dormant (CT104, 18 Oct: 14 % of the week left, shown as dormant).
func TestAccountWithARunningWindowIsNotDormant(t *testing.T) {
	now := time.Date(2026, 10, 18, 13, 34, 0, 0, time.UTC)
	full, left := 100.0, 14.0
	read := observation.Field{At: now, Source: observation.APISource, Quality: observation.Measured}
	short := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &full, Dormant: true, Usage: read, StatusEvidence: observation.Field{At: now, Source: observation.APISource, Quality: observation.Measured, Raw: "null"}}
	week := observation.Window{Provider: "claude", Scope: "all", Seconds: 604800, Remaining: &left, Reset: now.Add(85 * time.Hour), Usage: read, ResetEvidence: observation.Field{At: now, Source: observation.APISource, Quality: observation.Absolute}}
	idle := short
	idle.Seconds = 604800
	stale := week
	stale.Usage.At = now.Add(-2 * time.Hour)
	for _, c := range []struct {
		windows []observation.Window
		want    string
	}{
		{[]observation.Window{short, week}, "current"},
		{[]observation.Window{week, short}, "current"},
		{[]observation.Window{short, idle}, "dormant"},
		{[]observation.Window{short, stale}, "stale"},
	} {
		if got := accountState("claude", c.windows, now); got != c.want {
			t.Fatalf("%v windows: account %s, want %s", len(c.windows), got, c.want)
		}
	}
}
