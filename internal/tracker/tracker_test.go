package tracker

import (
	"database/sql"
	_ "github.com/mattn/go-sqlite3"
	"path/filepath"
	"testing"
	"time"
)

func TestForcedSwitchCheckpointSurvivesRestartAndAuxiliaries(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s, err := Open(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	q := Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "claude", Request: "r1", Kind: "primary", Capability: Digest("secret"), Auto: true, CompactOnSwitch: true, Checkpoint: "old"}
	key := Key("caller", "claude", q)
	a, b := Selection{Auth: "a", Account: "a", Alias: "A", Provider: "claude"}, Selection{Auth: "b", Account: "b", Alias: "B", Provider: "claude", Forced: true}
	if _, err = s.Admit("1", key, a, &q, true); err != nil {
		t.Fatal(err)
	}
	s.Complete("1")
	s, err = Open(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Admit("2", key, b, &q, true); err == nil {
		t.Fatal("forced move admitted uncompacted context")
	}
	q.Kind = "compaction"
	if _, err = s.Admit("summary", key, b, &q, true); err != nil {
		t.Fatal(err)
	}
	current, _, _ := s.Get(key)
	if current.Account != "a" {
		t.Fatal("summary rebound chat")
	}
	q.Kind = "primary"
	q.Checkpoint = "new"
	current, err = s.Admit("3", key, b, &q, true)
	if err != nil || current.Account != "b" || current.Generation != 2 {
		t.Fatal(current, err)
	}
	q.Capability = Digest("foreign")
	if _, err = s.Admit("4", key, b, &q, true); err == nil {
		t.Fatal("foreign capability admitted")
	}
	if _, err = s.ByCapability("foreign", "claude"); err == nil {
		t.Fatal("foreign capability read")
	}
	if _, err = s.ByCapability("secret", "wrong-route"); err == nil {
		t.Fatal("cross-route read")
	}
}

func TestColdMoveNeverCompactsAndWarmthUsesRequestStart(t *testing.T) {
	db, _ := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.sqlite"))
	defer db.Close()
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	now := start
	s, err := Open(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	q := Claim{Protocol: 1, Installation: "i", Session: "s", Route: "r", Request: "q", Kind: "primary", Capability: Digest("secret"), Auto: true, CompactOnSwitch: true}
	key := Key("caller", "claude", q)
	if _, err = s.Admit("a", key, Selection{Auth: "a", Account: "a"}, &q, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if err = s.CacheActivity("a", false, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, active, _ := s.Get(key)
	if !active || !b.UnknownWarmth {
		t.Fatal("failed stream changed cache", b)
	}
	if err = s.CacheActivity("a", true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, _ = s.Get(key)
	if !b.ProtectedUntil.Equal(start.Add(time.Hour)) {
		t.Fatal("response completion extended TTL", b)
	}
	s.Complete("a")
	if _, err = s.Admit("b", key, Selection{Auth: "b", Account: "b", Forced: false}, &q, true); err != nil {
		t.Fatal("cold move compacted", err)
	}
	b, _, _ = s.Get(key)
	if !b.ProtectedUntil.IsZero() || !b.UnknownWarmth {
		t.Fatal("account move transferred cache", b)
	}
}
