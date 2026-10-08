package history

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReportMaterializationPublicationContentAndIntegrity(t *testing.T) {
	ctx := context.Background()
	o := fixture(t, func(db *sql.DB) {
		mustExec(t, db, `CREATE TRIGGER archive_only BEFORE INSERT ON analytics_events BEGIN SELECT RAISE(ABORT,'source SQL must not execute in report view'); END`)
	})
	s := store(t)
	o.MaxBatches = 1
	if _, e := s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "reports.sqlite")
	if _, e := s.MaterializeReports(ctx, path); !errors.Is(e, ErrPending) {
		t.Fatal(e)
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("partial archive materialized")
	}
	o.MaxBatches = 0
	if _, e := s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	boundary, e := s.MaterializeReports(ctx, path)
	if e != nil || boundary != o.Boundary {
		t.Fatal(boundary, e)
	}
	db, e := sql.Open("sqlite3", dsn(path, "mode=ro&_query_only=1"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var revision, observation int64
	var raw sql.NullString
	if db.QueryRow(`SELECT revision FROM analytics_profile_revisions ORDER BY revision DESC LIMIT 1`).Scan(&revision) != nil || revision != 7 {
		t.Fatal("revision lost")
	}
	if db.QueryRow(`SELECT rowid FROM analytics_observations WHERE id='obs-1'`).Scan(&observation) != nil || observation != 1 {
		t.Fatal("observation pin moved")
	}
	if db.QueryRow(`SELECT raw_json FROM usage_events WHERE id=1`).Scan(&raw) != nil || raw.Valid {
		t.Fatal("raw credentials materialized")
	}
	var triggers int
	if db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='trigger'`).Scan(&triggers) != nil || triggers != 0 {
		t.Fatal("executed archival trigger SQL")
	}
	if _, e = s.MaterializeReports(ctx, path); !errors.Is(e, ErrInput) {
		t.Fatal("overwrote existing destination", e)
	}
	mustExec(t, s.db, `UPDATE history_tables SET chain='corrupt' WHERE name='analytics_events'`)
	bad := filepath.Join(t.TempDir(), "bad.sqlite")
	if _, e = s.MaterializeReports(ctx, bad); !errors.Is(e, ErrDigest) {
		t.Fatal("ignored archival chain mismatch", e)
	}
	if _, e = os.Stat(bad); !os.IsNotExist(e) {
		t.Fatal("failed output left published")
	}
}
