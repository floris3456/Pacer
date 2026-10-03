package handoff

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
)

func switchEngine(t *testing.T, gate bool) *Engine {
	t.Helper()
	e, err := Open(Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), Adaptive: adaptive.Config{Mode: "shadow"}, Credentials: []Credential{
		{ID: "claude-a", Index: "a", Provider: "claude", Account: "a", Alias: "Claude A"},
		{ID: "claude-b", Index: "b", Provider: "claude", Account: "b", Alias: "Claude B"},
	}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.SetCompactOnSwitch(gate)
	return e
}

// One chat's claim: an existing conversation (not fresh) from a client that
// declares it compacts on a switch refusal.
func switchClaim(request, checkpoint string) *Claim {
	q, _ := fakeClaim(7)
	q.Request, q.Fresh, q.Checkpoint, q.CompactOnSwitch = request, false, checkpoint, true
	return q
}

// observe is the after-auth call CPA makes once per credential attempt.
func observe(e *Engine, auth string, q *Claim) (Flight, error) {
	f, err := e.Pick("caller", "claude", "light", []string{auth}, q, adaptive.Request{ID: q.Request, Model: "light", Kind: q.Kind})
	if err == nil {
		e.Complete(f.Admission)
	}
	return f, err
}

func TestCompactOnSwitchRefusesTheMoveUntilAFreshCheckpoint(t *testing.T) {
	e := switchEngine(t, true)
	_, capability := fakeClaim(7)
	route := switchClaim("", "").Route
	if f, err := observe(e, "claude-a", switchClaim("r1", "cmp0")); err != nil || f.Auth != "claude-a" || f.Generation != 1 {
		t.Fatalf("first request not bound to A: %+v %v", f, err)
	}
	// CPA selected B (A used up or unavailable): refused before B receives anything.
	for _, checkpoint := range []string{"", "cmp0"} { // none, or one A already received
		_, err := observe(e, "claude-b", switchClaim("r2"+checkpoint, checkpoint))
		refusal, ok := err.(*Refusal)
		if !ok || refusal.Code != "quota_compact_required" || !strings.Contains(refusal.Message, "from Claude A to Claude B") {
			t.Fatalf("switch without compaction admitted (checkpoint %q): %v", checkpoint, err)
		}
	}
	if s, err := e.Snapshot(capability, 0, route); err != nil || s.Alias != "Claude A" || s.Generation != 1 {
		t.Fatalf("refusal moved the binding: %+v %v", s, err)
	}
	// The summary (or a title) may run on B; the chat stays bound to A.
	for _, kind := range []string{"compaction", "title"} {
		aux := switchClaim("aux-"+kind, "")
		aux.Kind = kind
		if f, err := observe(e, "claude-b", aux); err != nil || f.Auth != "claude-b" || f.Generation != 1 {
			t.Fatalf("%s on the new account refused or moved the chat: %+v %v", kind, f, err)
		}
	}
	if s, _ := e.Snapshot(capability, 0, route); s.Alias != "Claude A" {
		t.Fatalf("auxiliary request moved the chat: %+v", s)
	}
	// The compacted chat carries a checkpoint A never received: it moves to B.
	f, err := observe(e, "claude-b", switchClaim("r3", "cmp1"))
	if err != nil || f.Auth != "claude-b" || f.Generation != 2 {
		t.Fatalf("compacted chat not switched: %+v %v", f, err)
	}
	s, err := e.Snapshot(capability, 0, route)
	if err != nil || s.Alias != "Claude B" || s.Generation != 2 || s.Consumed != "cmp1" {
		t.Fatalf("binding after switch: %+v %v", s, err)
	}
	if last := s.Events[len(s.Events)-1]; last.Kind != "switched" || !strings.Contains(last.Text, "Claude A to Claude B") {
		t.Fatalf("switch not recorded: %+v", s.Events)
	}
	// Staying on B needs nothing; the next move (back to A) is gated again.
	if _, err := observe(e, "claude-b", switchClaim("r4", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := observe(e, "claude-a", switchClaim("r5", "")); err == nil {
		t.Fatal("move back to A admitted without compaction")
	}
}

func TestCompactOnSwitchLeavesOtherMovesAsBefore(t *testing.T) {
	cases := map[string]struct {
		gate  bool
		claim func(*Claim)
	}{
		"gate off":                   {false, func(*Claim) {}},
		"client does not declare it": {true, func(q *Claim) { q.CompactOnSwitch = false }},
		"automatic compaction off":   {true, func(q *Claim) { q.Auto = false }},
		"new chat":                   {true, func(q *Claim) { q.Fresh = true }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := switchEngine(t, c.gate)
			first := switchClaim("r1", "")
			c.claim(first)
			if _, err := observe(e, "claude-a", first); err != nil {
				t.Fatal(err)
			}
			next := switchClaim("r2", "")
			c.claim(next)
			f, err := observe(e, "claude-b", next)
			if err != nil || f.Auth != "claude-b" || f.Generation != 2 {
				t.Fatalf("move changed behavior: %+v %v", f, err)
			}
		})
	}
	// Ordinary clients (no claim) are never refused.
	e := switchEngine(t, true)
	if f, err := e.Pick("caller", "claude", "light", []string{"claude-b"}, nil, adaptive.Request{ID: "plain", Model: "light"}); err != nil || f.Auth != "claude-b" {
		t.Fatalf("ordinary request refused: %+v %v", f, err)
	}
}
