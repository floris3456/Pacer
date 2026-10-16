package pacer

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A Pacer version that adds one of its own tables changes the database's DDL
// generation before the guarded report feed captures it. The next start used to
// refuse registration ("measurement report feed integrity unavailable") on a
// database whose feed objects were intact, which took Pacer out of a running
// host. Production hit this on 2026-10-15 with the new pacer_settings table.
func TestStartAfterAnotherPacerVersionAddedATable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	startPlugin(t, path).Close()
	probe, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = probe.Exec(`CREATE TABLE pacer_settings_next(name TEXT PRIMARY KEY, configured TEXT, effective TEXT)`); err != nil {
		t.Fatal(err)
	}
	before := probe.QueryRow(`SELECT schema_cookie,(SELECT schema_version FROM pragma_schema_version) FROM measurement_report_meta WHERE id=1`)
	var saved, current int64
	if err = before.Scan(&saved, &current); err != nil {
		t.Fatal(err)
	}
	if saved == current {
		t.Fatalf("fixture did not move the DDL generation: %d", saved)
	}
	probe.Close()
	// The upgrade starts again: the feed objects are intact, so the new table
	// must not read as an unrecognized transition.
	next := startPlugin(t, path)
	defer next.Close()
	after, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	var settled, now int64
	if err = after.QueryRow(`SELECT schema_cookie,(SELECT schema_version FROM pragma_schema_version) FROM measurement_report_meta WHERE id=1`).Scan(&settled, &now); err != nil {
		t.Fatal(err)
	}
	if settled != now {
		t.Fatalf("new start left the feed anchored at %d of %d", settled, now)
	}
	// The rejection still stands where the feed's own objects are gone.
	missing := filepath.Join(t.TempDir(), "state.sqlite")
	startPlugin(t, missing).Close()
	gone, err := sql.Open("sqlite3", missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = gone.Exec(`DROP TRIGGER measurement_report_revision_insert`); err != nil {
		t.Fatal(err)
	}
	gone.Close()
	refused := New(testHost)
	raw, err := registerRaw(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = refused.Handle("plugin.register", raw); err == nil || !strings.Contains(err.Error(), "report feed") {
		t.Fatalf("a lost feed object was accepted: %v", err)
	}
}
