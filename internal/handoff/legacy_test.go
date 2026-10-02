package handoff

import (
	"strings"
	"testing"
)

func TestLegacyCanReuseConfirmedResetWithoutCheckpoint(t *testing.T) {
	l := setup(t)
	l.observe(t, "small", 10, 100, 118000)
	if f, err := l.e.Pick("legacy", "claude", "claude-sonnet", []string{"small", "large"}, nil); err != nil || f.Auth != "large" {
		t.Fatal(f, err)
	}
	l.now = 118001
	l.observe(t, "small", 100, 100, 136000)
	l.observe(t, "large", 100, 100, 136000)
	if f, err := l.e.Pick("legacy", "claude", "claude-sonnet", []string{"small", "large"}, nil); err != nil || f.Auth != "small" {
		t.Fatal("recovered account unavailable to ordinary client", f, err)
	}
	if len(l.e.state.Bindings) != 0 {
		t.Fatal("invented a legacy chat binding")
	}
	l.observe(t, "small", 100, 5, 136000)
	if f, err := l.e.Pick("legacy", "claude", "claude-sonnet", []string{"small", "large"}, nil); err != nil || f.Auth != "large" {
		t.Fatal("weekly hold bypassed", f, err)
	}
}

func TestLegacyWarningsOnlyDescribeFreshSelectedAccount(t *testing.T) {
	l := setup(t)
	l.observe(t, "small", 20, 15, 118000)
	warnings := l.e.LegacyWarnings("small-copy", "claude-sonnet")
	if len(warnings) != 2 {
		t.Fatal(warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w.Text, "Small copy") || !strings.Contains(w.Text, "resets ") || strings.Contains(w.Text, "compact") {
			t.Fatal(w)
		}
	}
	if len(l.e.LegacyWarnings("large", "claude-sonnet")) != 0 || len(l.e.LegacyWarnings("missing", "claude-sonnet")) != 0 {
		t.Fatal("unrelated account warned")
	}
	l.observe(t, "small", 10, 5, 118000)
	for _, w := range l.e.LegacyWarnings("small", "claude-sonnet") {
		if w.Kind != "handoff" {
			t.Fatal(w)
		}
	}
	l.now += 91
	if len(l.e.LegacyWarnings("small", "claude-sonnet")) != 0 {
		t.Fatal("stale percentage emitted")
	}
}
