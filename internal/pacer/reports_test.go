package pacer

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

func reportFixture(t *testing.T) *historyreports.Reader {
	return reportFixtureFrom(t, "golden/fixture.sql.gz", 40, 18)
}

func reportFixtureFrom(t *testing.T, fixture string, events, observations int64) *historyreports.Reader {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "frozen.sqlite")
	f, err := os.Open("../historyreports/testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	sqlBytes, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(sqlBytes)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(source, 0400); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	archive, err := history.Open(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	boundary := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hex.EncodeToString(digest[:]), LineageSHA256: strings.Repeat("a", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790812800000, EventsThrough: events, ObservationsThrough: observations}
	progress, err := archive.Import(context.Background(), history.ImportOptions{SourcePath: source, Boundary: boundary, BatchSize: 25})
	if err != nil || !progress.Complete {
		t.Fatal(progress, err)
	}
	reader, err := historyreports.Prepare(context.Background(), archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

// usageFixture is a plugin with the extended synthetic history (five
// overlapping profiles, keys, measured windows) prepared with quota facts.
func usageFixture(t *testing.T) *Plugin {
	t.Helper()
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	reader := reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	p.mu.Lock()
	p.reports = reader // the usage record's loop reads it under p.mu
	p.mu.Unlock()
	t.Cleanup(p.Close)
	return p
}
