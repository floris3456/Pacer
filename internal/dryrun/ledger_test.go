package dryrun

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time           { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openLedgerDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func openLedger(t *testing.T, db *sql.DB, c *testClock) *Ledger {
	t.Helper()
	l, err := OpenLedger(db, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mark(t *testing.T, l *Ledger, mode string, errors int64, providers ...string) {
	t.Helper()
	if err := l.Mark(mode, providers, errors); err != nil {
		t.Fatal(err)
	}
}

func coverage(t *testing.T, l *Ledger, from, to time.Time) (map[string][]Interval, []Interval) {
	t.Helper()
	c, e, err := l.Coverage(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	return c, e
}

func TestLedgerSecretIsPersistentPerInstallation(t *testing.T) {
	dir := t.TempDir()
	c := &testClock{t: day}
	db := openLedgerDB(t, filepath.Join(dir, "evidence.sqlite"))
	first := openLedger(t, db, c).Secret()
	if len(first) != 32 || bytes.Equal(first, make([]byte, 32)) {
		t.Fatal("secret must be 32 random bytes", first)
	}
	if again := openLedger(t, db, c).Secret(); !bytes.Equal(first, again) {
		t.Fatal("secret changed on reopen")
	}
	other := openLedger(t, openLedgerDB(t, filepath.Join(dir, "other.sqlite")), c).Secret()
	if bytes.Equal(first, other) {
		t.Fatal("two installations share a secret")
	}
}

func TestLedgerCoverageFromMarks(t *testing.T) {
	c := &testClock{t: day}
	db := openLedgerDB(t, filepath.Join(t.TempDir(), "evidence.sqlite"))
	run1 := openLedger(t, db, c)
	mark(t, run1, "dry-run", 0, "claude", "codex") // 00:00
	c.advance(30 * time.Second)
	mark(t, run1, "dry-run", 0, "claude") // 00:00:30 codex leaves routing
	c.advance(30 * time.Second)
	mark(t, run1, "off", 0, "claude") // 00:01 mode off
	c.advance(time.Minute)
	mark(t, run1, "dry-run", 0, "claude") // 00:02
	c.advance(30 * time.Second)
	mark(t, run1, "dry-run", 2, "claude") // 00:02:30 two decision writes failed
	c.advance(30 * time.Second)
	mark(t, run1, "dry-run", 2, "claude") // 00:03
	c.advance(MaxMarkGap + time.Second)
	mark(t, run1, "dry-run", 2, "claude") // stalled worker: no proof across the gap
	c.advance(30 * time.Second)
	mark(t, run1, "dry-run", 2, "claude")
	// A restart is a new run: the old run's last mark proves nothing after it.
	c.advance(10 * time.Second)
	run2 := openLedger(t, db, c)
	mark(t, run2, "dry-run", 0, "claude")
	c.advance(30 * time.Second)
	mark(t, run2, "dry-run", 0, "claude")

	cov, errs := coverage(t, run2, day.Add(-time.Hour), day.Add(time.Hour))
	s := func(d time.Duration) time.Time { return day.Add(d) }
	gap := MaxMarkGap + time.Second
	want := []Interval{
		{s(0), s(time.Minute)},
		{s(2 * time.Minute), s(3 * time.Minute)},
		{s(3*time.Minute + gap), s(3*time.Minute + gap + 30*time.Second)},
		{s(3*time.Minute + gap + 40*time.Second), s(3*time.Minute + gap + 70*time.Second)},
	}
	if fmt.Sprint(cov["claude"]) != fmt.Sprint(want) {
		t.Fatalf("claude coverage:\n got %v\nwant %v", cov["claude"], want)
	}
	if fmt.Sprint(cov["codex"]) != fmt.Sprint([]Interval{{s(0), s(30 * time.Second)}}) {
		t.Fatalf("codex coverage: %v", cov["codex"])
	}
	if fmt.Sprint(errs) != fmt.Sprint([]Interval{{s(2 * time.Minute), s(2*time.Minute + 30*time.Second)}}) {
		t.Fatalf("error segments: %v", errs)
	}
	// The read window clips nothing itself but excludes marks far outside it.
	if cov, _ = coverage(t, run2, day.Add(2*time.Hour), day.Add(3*time.Hour)); len(cov) != 0 {
		t.Fatalf("coverage outside the window: %v", cov)
	}
}

func TestLedgerPruneKeepsRecentMarks(t *testing.T) {
	c := &testClock{t: day}
	l := openLedger(t, openLedgerDB(t, filepath.Join(t.TempDir(), "evidence.sqlite")), c)
	mark(t, l, "dry-run", 0, "claude")
	c.advance(30 * time.Second)
	mark(t, l, "dry-run", 0, "claude")
	c.advance(40 * 24 * time.Hour)
	mark(t, l, "dry-run", 0, "claude")
	c.advance(30 * time.Second)
	mark(t, l, "dry-run", 0, "claude")
	if err := l.Prune(c.t.Add(-35 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if cov, _ := coverage(t, l, day.Add(-time.Hour), day.Add(time.Hour)); len(cov) != 0 {
		t.Fatalf("pruned marks still prove coverage: %v", cov)
	}
	if cov, _ := coverage(t, l, c.t.Add(-time.Hour), c.t.Add(time.Hour)); len(cov["claude"]) != 1 {
		t.Fatalf("recent marks lost: %v", cov)
	}
}
