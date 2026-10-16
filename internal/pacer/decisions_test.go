package pacer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
)

func TestDecisionTracePreservesActualVersusProposedWithoutPrivateHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	if err := p.setMode("on"); err != nil {
		t.Fatal(err)
	}
	ticket := attempt{Key: "opaque-chat-digest", Caller: "private-caller", Session: "private-session", Model: "claude-opus-5-5", PreviousAccount: "prior-account", Decision: routing.Decision{Auth: "a", Account: p.creds["a"].Account, Handled: true, Reason: "cold_move"}}
	for i := 0; i < 210; i++ {
		p.recordDecision("request", ticket, p.creds["b"], "admitted")
	}
	p.Close()
	p = startPlugin(t, path)
	defer p.Close()
	rows, err := p.decisionView()
	if err != nil || len(rows) != 200 {
		t.Fatal(len(rows), err)
	}
	if rows[0].ActualAuth != "b" || rows[0].Proposed.Auth != "a" || rows[0].At.IsZero() {
		t.Fatal(rows[0])
	}
	body, _ := json.Marshal(rows)
	if strings.Contains(string(body), "private-") {
		t.Fatal("private request identity escaped to decisions")
	}
	if err = p.setMode("off"); err != nil {
		t.Fatal(err)
	}
	p.recordDecision("request", ticket, p.creds["a"], "admitted")
	var count int
	if err = p.db.QueryRow(`SELECT count(*) FROM pacer_decisions`).Scan(&count); err != nil || count != 210 {
		t.Fatal(count, err)
	}
}
