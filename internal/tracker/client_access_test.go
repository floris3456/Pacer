package tracker

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCallerOwnershipSurvivesRestartAndCannotBeReassigned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	open := func() (*sql.DB, *Store) {
		t.Helper()
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Open(db, func() time.Time { return now })
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		return db, s
	}
	db, s := open()
	q := Claim{Protocol: 1, Installation: "install", Session: "chat", Route: "claude", Request: "request", Kind: "primary", Capability: Digest("private-capability")}
	owner := Digest("caller-one")
	key := Key(owner, "claude", q)
	selection := Selection{Caller: owner, Auth: "a", Account: "account-a", Provider: "claude", Model: "claude-opus-5-5"}
	if _, err := s.Admit("first", key, selection, &q, false); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	db, s = open()
	defer db.Close()
	persisted, err := s.ByCapability("private-capability", q.Route)
	if err != nil || persisted.Caller != owner {
		t.Fatal("caller not persisted with capability", persisted, err)
	}
	selection.Caller = Digest("caller-two")
	for _, kind := range []string{"primary", "compaction"} {
		q.Kind = kind
		_, err := s.Admit("foreign-"+kind, key, selection, &q, false)
		var refusal *Refusal
		if !errors.As(err, &refusal) || refusal.Code != "quota_identity_conflict" {
			t.Fatal("foreign caller reassigned binding", kind, err)
		}
	}
	q.Kind = "primary"
	selection.Caller = ""
	if _, err = s.Admit("older-internal-caller", key, selection, &q, false); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(key)
	if err != nil || got.Caller != owner {
		t.Fatal("missing caller erased persisted owner", got, err)
	}
}
