package prefixevidence

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// sweepOnce runs exactly one sweep() call in its own transaction, so the
// per-sweep deletion cap is measured without the two sweeps Observe/Settle add.
func sweepOnce(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := sweep(ctx, tx, now.UnixNano()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func remainingIDs(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT attempt_id FROM prefix_attempts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

// Retention (a): anchors older than 2h by primary start are deleted. A warming
// attempt arriving after its anchor was swept gets no_primary_anchor. A fresher
// anchor with an unrelated tag survives.
func TestRetentionDeletesOldAnchors(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.settle(t, "att-1", OutcomeSucceeded)
	if n := countRows(t, h.db, "prefix_anchors"); n != 1 {
		t.Fatalf("anchors=%d before retention, want 1", n)
	}

	h.at(2*time.Hour + time.Second)
	fresh := baseFact(KindPrimary, "5m")
	fresh.AttemptID = "att-fresh"
	fresh.StartedAt = t0.Add(time.Hour)
	fresh.Snapshot.Endpoints[0].Tag = hex64('f')
	h.clk.Set(fresh.StartedAt)
	h.observe(t, fresh)
	h.settle(t, "att-fresh", OutcomeSucceeded)
	h.at(2*time.Hour + time.Second)
	if err := h.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.db, "prefix_anchors"); n != 1 {
		t.Fatalf("anchors=%d after retention, want only the fresh one", n)
	}

	w := baseFact(KindWarming, "5m")
	w.AttemptID = "warm-1"
	w.StartedAt = t0.Add(time.Minute)
	h.observe(t, w)
	h.settle(t, "warm-1", OutcomeSucceeded)
	if s, r, _ := obsRow(t, h.db, "warm-1"); s != "unverified" || r != "no_primary_anchor" {
		t.Fatalf("warming after swept anchor = %s/%s, want unverified/no_primary_anchor", s, r)
	}
}

// Retention (b): terminal rows older than 7 days by start are deleted, except
// the newest row per (scope, epoch). A fresh row always survives. Anchors of the
// deleted attempts were already removed by rule (a).
func TestRetentionDeletesOldTerminalKeepsNewestPerScopeEpoch(t *testing.T) {
	h := newHarness(t)
	add := func(id, epoch string, start time.Duration) {
		t.Helper()
		f := baseFact(KindPrimary, "5m")
		f.AttemptID = id
		f.Epoch = epoch
		f.StartedAt = t0.Add(start)
		h.clk.Set(f.StartedAt)
		h.observe(t, f)
		h.settle(t, id, OutcomeSucceeded)
	}
	add("a1", "epoch-1", 0)
	add("a2", "epoch-1", time.Minute)
	add("a3", "epoch-1", 2*time.Minute)
	add("b1", "epoch-2", 0)
	add("a5", "epoch-2", 6*24*time.Hour)

	h.at(7*24*time.Hour + time.Hour)
	if err := h.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := remainingIDs(t, h.db)
	want := map[string]bool{"a3": true, "a5": true}
	if len(got) != len(want) || !got["a3"] || !got["a5"] {
		t.Fatalf("remaining=%v, want exactly %v (a3 newest of epoch-1, a5 fresh)", got, want)
	}
	// Every primary here started more than 2h ago, so rule (a) removed all anchors.
	if n := countRows(t, h.db, "prefix_anchors"); n != 0 {
		t.Fatalf("anchors=%d, want 0 (all primary starts older than 2h)", n)
	}
}

// Retention never deletes a pending attempt. A fact or settlement that has
// not yet timed out is pending no matter how old its start is.
func TestRetentionNeverDeletesPending(t *testing.T) {
	h := newHarness(t)
	h.at(7 * 24 * time.Hour)
	f := baseFact(KindPrimary, "5m") // start t0, 7 days old, arrives now
	h.observe(t, f)
	h.settle(t, "settle-only", OutcomeSucceeded) // outcome-only, arrives now

	h.at(7*24*time.Hour + time.Minute)
	if err := h.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := remainingIDs(t, h.db)
	if !got["att-1"] {
		t.Fatalf("pending fact deleted: remaining=%v", got)
	}
	if s, r, _ := obsRow(t, h.db, "att-1"); s != "unverified" || r != "pending_settlement" {
		t.Fatalf("pending fact = %s/%s, want unverified/pending_settlement", s, r)
	}
	if !got["settle-only"] {
		t.Fatalf("pending settlement deleted: remaining=%v", got)
	}
}

// Retention (c): one sweep deletes at most 1000 rows in total. Anchors go first
// (oldest first), and attempts share the remaining budget.
func TestRetentionCapsDeletionsPerSweep(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		start := t0.Add(time.Duration(i) * time.Second).UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT INTO prefix_attempts(attempt_id, fact, outcome, arrived_ns, started_ns, caller, installation, session, route, epoch) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("old-%04d", i), "{}", "succeeded", start, start, caller, "inst-1", "sess-1", "route-1", "epoch-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO prefix_anchors(account, binding, key_id, settings_tag, caller, installation, session, route, epoch, tag, started_ns, attempt_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			"acct-1", "bind-1", "k", "s", caller, "inst-1", "sess-1", "route-1", "epoch-1", fmt.Sprintf("%064x", i), start, fmt.Sprintf("old-%04d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	now := t0.Add(8 * 24 * time.Hour)
	sweepOnce(t, h.db, now)
	if n := countRows(t, h.db, "prefix_anchors"); n != 500 {
		t.Fatalf("after sweep 1 anchors=%d, want 500 (1000 deleted, anchors first)", n)
	}
	if n := countRows(t, h.db, "prefix_attempts"); n != 1500 {
		t.Fatalf("after sweep 1 attempts=%d, want 1500 (budget spent on anchors)", n)
	}

	sweepOnce(t, h.db, now)
	if n := countRows(t, h.db, "prefix_anchors"); n != 0 {
		t.Fatalf("after sweep 2 anchors=%d, want 0", n)
	}
	if n := countRows(t, h.db, "prefix_attempts"); n != 1000 {
		t.Fatalf("after sweep 2 attempts=%d, want 1000 (500 anchors + 500 attempts = budget)", n)
	}
	// Oldest first: the survivors are the newest attempts.
	got := remainingIDs(t, h.db)
	if got["old-0000"] || !got["old-1499"] {
		t.Fatalf("deletion was not oldest-first")
	}
}

// The newest attempt of a scope survives even when every row is old, so readback
// still has something to show after retention.
func TestRetentionKeepsNewestOfOnlyScope(t *testing.T) {
	h := newHarness(t)
	h.at(0)
	h.observe(t, baseFact(KindPrimary, "5m"))
	h.settle(t, "att-1", OutcomeSucceeded)
	h.at(8 * 24 * time.Hour)
	if err := h.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !remainingIDs(t, h.db)["att-1"] {
		t.Fatalf("newest row of its only scope was deleted")
	}
}
