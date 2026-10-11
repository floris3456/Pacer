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

func TestByCapabilityRequiresUniqueBindingWithinRoute(t *testing.T) {
	for _, scope := range []string{"different-caller", "different-session"} {
		t.Run(scope, func(t *testing.T) {
			db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			s, err := Open(db, clock)
			if err != nil {
				t.Fatal(err)
			}
			const token = "client-persisted-capability-for-this-session"
			q := Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "claude/opus", Request: "request", Kind: "primary", Capability: Digest(token)}
			first, err := s.Admit("first", Key("caller", "claude", q), Selection{Auth: "a", Account: "a", Provider: "claude"}, &q, true)
			if err != nil {
				t.Fatal(err)
			}
			other := q
			other.Route = "claude/sonnet"
			separateRoute, err := s.Admit("other-route", Key("caller", "claude", other), Selection{Auth: "c", Account: "c", Provider: "claude"}, &other, true)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := s.ByCapability(token, q.Route); err != nil || got != first {
				t.Fatalf("unique protocol 1 capability failed: %#v, %v", got, err)
			}
			for _, lookup := range [][2]string{{"unknown-token", q.Route}, {token, "unknown-route"}} {
				if got, err := s.ByCapability(lookup[0], lookup[1]); err == nil || got != (Binding{}) {
					t.Fatalf("unknown capability or route returned a binding: %#v, %v", got, err)
				}
			}
			caller := "caller"
			if scope == "different-caller" {
				caller = "another-caller"
			} else {
				q.Session = "another-chat"
			}
			second, err := s.Admit("second", Key(caller, "claude", q), Selection{Auth: "b", Account: "b", Provider: "claude"}, &q, true)
			if err != nil || first.Key == second.Key {
				t.Fatalf("distinct binding fixture failed: %#v, %v", second, err)
			}
			// Existing databases may already contain duplicates. Reopening must
			// reject their lookup without changing admission or storage format.
			s, err = Open(db, clock)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := s.ByCapability(token, q.Route); err == nil || got != (Binding{}) {
				t.Fatalf("ambiguous capability returned a caller's binding: %#v, %v", got, err)
			}
			if got, err := s.ByCapability(token, other.Route); err != nil || got != separateRoute {
				t.Fatalf("ambiguity crossed into another route: %#v, %v", got, err)
			}
		})
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

func TestLateUsageFromOldCheckpointCannotWarmCompactedPrefix(t *testing.T) {
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
	q := Claim{Protocol: 1, Installation: "i", Session: "s", Route: "r", Request: "q", Kind: "primary", Capability: Digest("secret"), Checkpoint: "old"}
	key := Key("caller", "claude", q)
	selection := Selection{Auth: "a", Account: "a"}
	if _, err = s.Admit("old-attempt", key, selection, &q, true); err != nil {
		t.Fatal(err)
	}
	q.Checkpoint = "new"
	if _, err = s.Admit("new-attempt", key, selection, &q, true); err != nil {
		t.Fatal(err)
	}
	if err = s.CacheActivity("old-attempt", true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Get(key)
	if err != nil || !b.UnknownWarmth || !b.ProtectedUntil.IsZero() {
		t.Fatal("old prefix refreshed new one", b, err)
	}
	if err = s.CacheActivity("new-attempt", true, true, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	b, _, err = s.Get(key)
	if err != nil || b.UnknownWarmth || !b.ProtectedUntil.Equal(now.Add(5*time.Minute)) {
		t.Fatal(b, err)
	}
}

func TestAsyncActivityAfterCompletionAndRestartUsesCapturedAccountCheckpointAndStart(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	now := start
	s, err := Open(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	q := Claim{Protocol: 1, Installation: "i", Session: "s", Route: "r", Request: "q", Kind: "primary", Capability: Digest("private"), Checkpoint: "old"}
	key := Key("caller", "claude", q)
	if _, err = s.Admit("logical", key, Selection{Auth: "a", Account: "a"}, &q, false); err != nil {
		t.Fatal(err)
	}
	s.Complete("logical")
	now = start.Add(20 * time.Minute)
	s, err = Open(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CacheActivityForAuth("logical", "a", start, true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Get(key)
	if err != nil || !b.ProtectedUntil.Equal(start.Add(time.Hour)) || b.UnknownWarmth {
		t.Fatal(b, err)
	}
	q.Checkpoint = "new"
	if _, err = s.Admit("next", key, Selection{Auth: "a", Account: "a"}, &q, false); err != nil {
		t.Fatal(err)
	}
	if err = s.CacheActivityForAuth("logical", "a", start, true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, err = s.Get(key)
	if err != nil || !b.ProtectedUntil.IsZero() || !b.UnknownWarmth {
		t.Fatal("late old checkpoint warmed replacement", b, err)
	}
	if err = s.CacheActivityForAuth("next", "b", now, true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, err = s.Get(key)
	if err != nil || !b.UnknownWarmth {
		t.Fatal("foreign account activity warmed context", b, err)
	}
	if err = s.CacheActivityForAuth("next", "a", now, false, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, err = s.Get(key)
	if err != nil || !b.UnknownWarmth {
		t.Fatal("failed activity warmed context", b, err)
	}
	if _, err = s.Admit("away", key, Selection{Auth: "b", Account: "b"}, &q, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Admit("back", key, Selection{Auth: "a", Account: "a"}, &q, false); err != nil {
		t.Fatal(err)
	}
	if err = s.CacheActivityForAuth("next", "a", now, true, true, time.Hour); err != nil {
		t.Fatal(err)
	}
	b, _, err = s.Get(key)
	if err != nil || !b.UnknownWarmth {
		t.Fatal("returning to an old account transferred old cache activity", b, err)
	}
}
