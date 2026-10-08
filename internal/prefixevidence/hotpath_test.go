package prefixevidence

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A primary settle re-resolves only the warming attempts that start inside the
// anchor's dependent window. Warming rows far outside it are never rewritten.
// The sentinel written into obs_reason survives only if resolve did not run.
// Warming rows are settled so they are dependent candidates at all.
func TestPrimarySettleLeavesWarmingOutsideWindowUntouched(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	for i := 0; i < 500; i++ {
		w := baseFact(KindWarming, "5m")
		w.AttemptID = fmt.Sprintf("far-%03d", i)
		w.StartedAt = t0.Add(3*time.Hour + time.Duration(i)*time.Millisecond)
		h.observe(t, w)
		h.settle(t, w.AttemptID, OutcomeSucceeded)
	}
	near := baseFact(KindWarming, "5m")
	near.AttemptID = "near"
	near.StartedAt = t0.Add(time.Hour)
	h.observe(t, near)
	h.settle(t, "near", OutcomeSucceeded)
	if _, err := h.db.Exec(`UPDATE prefix_attempts SET obs_reason='sentinel' WHERE kind='warming'`); err != nil {
		t.Fatal(err)
	}
	h.observe(t, baseFact(KindPrimary, "5m")) // primary: attempt-1, start t0
	h.settle(t, "att-1", OutcomeSucceeded)

	var far, inWindow int
	if err := h.db.QueryRow(`SELECT count(*) FROM prefix_attempts WHERE attempt_id LIKE 'far-%' AND obs_reason='sentinel'`).Scan(&far); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT count(*) FROM prefix_attempts WHERE attempt_id='near' AND obs_reason='sentinel'`).Scan(&inWindow); err != nil {
		t.Fatal(err)
	}
	if far != 500 {
		t.Fatalf("%d of 500 out-of-window warming rows were re-resolved", 500-far)
	}
	// Control: the in-window row must have been re-resolved, or the window is not exercised at all.
	if inWindow != 0 {
		t.Fatal("in-window warming row was not re-resolved by the primary settle")
	}
}

// Write transactions take the write lock up front (BEGIN IMMEDIATE), as Open
// does. A deferred transaction holds no lock yet when the clock is read inside
// it, so a second connection could still start a write there.
func TestWriteTransactionsBeginImmediate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefix.sqlite")
	probe, err := sql.Open("sqlite3", path+"?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { probe.Close() })
	db := openDB(t, path)

	probed := false
	var probeErr error
	clk := func() time.Time {
		if !probed {
			probed = true
			probeErr = tryWriteLock(probe)
		}
		return t0
	}
	s, err := Open(db, clk)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(context.Background(), baseFact(KindPrimary, "5m")); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Fatal("clock never read inside the write transaction")
	}
	if probeErr == nil {
		t.Fatal("write transaction did not hold the write lock while the clock was read")
	}
}

// tryWriteLock asks for the write lock from another connection without waiting.
func tryWriteLock(db *sql.DB) error {
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "ROLLBACK")
	return err
}

// The pending predicate and the terminal ordering have indexes, and the pending
// query uses its partial index.
func TestPendingAndTerminalQueriesHaveIndexes(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"prefix_attempts_pending", "prefix_attempts_terminal"} {
		var n int
		if err := h.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("index %s missing", name)
		}
	}
	rows, err := h.db.Query(`EXPLAIN QUERY PLAN SELECT attempt_id FROM prefix_attempts WHERE closed='' AND ((fact<>'') <> (outcome<>'')) AND arrived_ns<=? ORDER BY arrived_ns, rowid`, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "prefix_attempts_pending") {
		t.Fatalf("pending sweep does not use its index:\n%s", plan.String())
	}
}
