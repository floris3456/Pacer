package handoff

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

func TestUsageScopeActualSelectionAndReadOnlyState(t *testing.T) {
	l := setup(t)
	l.e.cfg.Credentials = append(l.e.cfg.Credentials, Credential{ID: "codex", Index: "4", Provider: "codex", Account: "codex", Alias: "Codex"})
	l.observe(t, "codex", 77, 88, 118000)
	if err := l.e.Observe("small", []quota.Window{{Scope: "opus", Seconds: 604800, Remaining: 42, Reset: 704800, Observed: l.now}}); err != nil {
		t.Fatal(err)
	}
	q := claim("a")
	f, err := l.pick(q)
	if err != nil {
		t.Fatal(err)
	}
	read := func(all bool) Usage {
		t.Helper()
		u, err := l.e.UsageView(strings.Repeat("a", 48), q.Route, all)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if u := read(false); u.Current != "" || len(u.Active) != 0 {
		t.Fatal("scheduler choice presented as actual usage", u)
	}
	_ = l.e.ConfirmUsage(f.Admission, "large")
	if read(false).Current != "" {
		t.Fatal("mismatched host credential accepted")
	}
	if err := l.e.ConfirmUsage(f.Admission, f.Auth); err != nil {
		t.Fatal(err)
	}
	q2 := claim("b")
	f2, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"large"}, &q2)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.e.ConfirmUsage(f2.Admission, f2.Auth)
	before, _ := l.e.MarshalState()
	u := read(false)
	if u.Current != "small" || len(u.Active) != 1 || u.Active[0] != "small" || len(u.Credentials) != 3 {
		t.Fatal(u)
	}
	for _, c := range u.Credentials {
		if c.Provider != "claude" || len(c.Windows) != 2 {
			t.Fatal("model filter included unrelated provider or scope", c)
		}
		if c.ID == "small" && (len(c.Shared) != 1 || c.Shared[0] != "small-copy") {
			t.Fatal("shared quota identity was lost", c)
		}
	}
	all := read(true)
	if len(all.Credentials) != 4 {
		t.Fatal("all omitted an unused provider", all)
	}
	for _, c := range all.Credentials {
		if c.ID == "small" && len(c.Windows) != 3 {
			t.Fatal("all omitted a model-specific window", c)
		}
	}
	if _, err := l.e.UsageView(strings.Repeat("a", 48), "another-route", true); err == nil {
		t.Fatal("route capability isolation failed")
	}
	if _, err := l.e.UsageView(strings.Repeat("x", 48), q.Route, true); err == nil {
		t.Fatal("unknown capability accepted")
	}
	after, _ := l.e.MarshalState()
	if string(before) != string(after) {
		t.Fatal("usage query modified activity, admission, notices or bindings")
	}
	encoded, _ := json.Marshal(all)
	for _, secret := range []string{q.Capability, f2.Key, "same-small-account"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("private routing state leaked")
		}
	}
	l.e.Complete(f.Admission)
	if u := read(false); len(u.Active) != 0 || u.Current != "small" || u.LastUsed != l.now {
		t.Fatal("idle view lost exact last-used credential", u)
	}
}

func TestUsageSurvivesRestartAndIgnoresAuxiliarySelection(t *testing.T) {
	l := setup(t)
	l.e.cfg.Adaptive.Mode = "shadow"
	q := claim("a")
	f, err := l.pick(q)
	// Shadow observes one host-selected credential, not a list of candidates.
	if err == nil {
		t.Fatal("invalid shadow fixture accepted")
	}
	f, err = l.e.Pick("caller", "claude", "claude-sonnet", []string{"small"}, &q)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.e.ConfirmUsage(f.Admission, f.Auth)
	l.e.Complete(f.Admission)
	q.Kind, q.Request = "title", "title"
	aux, err := l.e.Pick("caller", "claude", "claude-sonnet", []string{"large"}, &q)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.e.ConfirmUsage(aux.Admission, aux.Auth)
	u, _ := l.e.UsageView(strings.Repeat("a", 48), q.Route, false)
	if u.Current != "small" || len(u.Active) != 0 {
		t.Fatal("title overwrote primary credential", u)
	}
	if err := l.e.Close(); err != nil {
		t.Fatal(err)
	}
	l.e, err = Open(l.cfg, func() time.Time { return time.Unix(l.now+10, 0) })
	if err != nil {
		t.Fatal(err)
	}
	u, err = l.e.UsageView(strings.Repeat("a", 48), q.Route, false)
	if err != nil || u.Current != "small" || len(u.Active) != 0 {
		t.Fatal("restart lost confirmed credential or invented a running request", u, err)
	}
}
