package capacity

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func openTestStore(t *testing.T) (*Store, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capacity.sqlite")
	db := openTestDB(t, path)
	t.Cleanup(func() { db.Close() })
	s, err := Open(db, func() time.Time { return epoch })
	if err != nil {
		t.Fatal(err)
	}
	return s, db, path
}

type ledgerRow struct {
	Account, Tuple                 string
	FromSeq, ThroughSeq            int64
	FromAt, ThroughAt, Incarnation string
}

func ledgerRows(t *testing.T, db *sql.DB) []ledgerRow {
	t.Helper()
	rows, err := db.Query(`SELECT account,tuple,from_seq,through_seq,from_at,through_at,incarnation FROM capacity_inventory ORDER BY tuple,from_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		var from, through int64
		if err = rows.Scan(&r.Account, &r.Tuple, &r.FromSeq, &r.ThroughSeq, &from, &through, &r.Incarnation); err != nil {
			t.Fatal(err)
		}
		r.FromAt, r.ThroughAt = time.Unix(0, from).UTC().Format(time.RFC3339), time.Unix(0, through).UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	return out
}

func put(t *testing.T, s *Store, c Checkpoint) Checkpoint {
	t.Helper()
	stored, err := s.PutCheckpoint(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID == 0 {
		t.Fatal("checkpoint not stored")
	}
	return stored
}

func TestLedgerRowsPerTupleIntervalAndSplitMembership(t *testing.T) {
	s, db, _ := openTestStore(t)
	a, b := tuple("a", "ref-A", 5, "active"), tuple("b", "ref-A", 6, "active")
	rotated := tuple("a", "ref-A", 25, "active")
	c10 := put(t, s, checkpoint("inv", 10, a, b))
	c20 := put(t, s, checkpoint("inv", 20, a, b))
	c30 := put(t, s, checkpoint("inv", 30, rotated, b))
	c40 := put(t, s, checkpoint("inv", 40, rotated, b))
	rows := ledgerRows(t, db)
	if len(rows) != 3 {
		t.Fatal("expected one row per unchanged tuple binding", rows)
	}
	for _, r := range rows {
		if r.Account != "ref-A" || r.Incarnation != "inv" {
			t.Fatal(r)
		}
		switch {
		case r.Tuple == b.Ref && (r.FromSeq != 10 || r.ThroughSeq != 40 || r.FromAt != epoch.Add(10*time.Minute).Format(time.RFC3339) || r.ThroughAt != epoch.Add(40*time.Minute).Format(time.RFC3339)):
			t.Fatal("unchanged alias not extended", r)
		case r.Tuple == a.Ref && r.FromSeq == 10 && r.ThroughSeq != 20:
			t.Fatal("rotation did not close the old binding", r)
		case r.Tuple == a.Ref && r.FromSeq == 30 && r.ThroughSeq != 40:
			t.Fatal("rotated binding row", r)
		}
	}
	ctx := context.Background()
	load := func(c Checkpoint) Checkpoint {
		got, err := s.Checkpoint(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := load(c20); got.Sequence != 20 || got.Coverage != "known" || len(got.Tuples) != 2 || !reflect.DeepEqual(got.Tuples, sortedTuples(a, b)) {
		t.Fatal("checkpoint not reconstructed from ledger rows", got)
	}
	if !JudgeMembership("ref-A", load(c10), load(c20)).Holds || JudgeMembership("ref-A", load(c20), load(c30)).Holds || !JudgeMembership("ref-A", load(c30), load(c40)).Holds {
		t.Fatal("ledger did not split around the rotation")
	}
}

func sortedTuples(ts ...Tuple) []Tuple {
	out := append([]Tuple(nil), ts...)
	sortTuples(out)
	return out
}

func TestLedgerDurableAcrossReopenAndUnknownCheckpoints(t *testing.T) {
	s, db, path := openTestStore(t)
	a := tuple("a", "ref-A", 5, "active")
	c := put(t, s, checkpoint("inv", 10, a))
	unknown := put(t, s, Checkpoint{Coverage: "unknown", Reason: "inventory_unsupported"})
	db.Close()
	db = openTestDB(t, path)
	defer db.Close()
	s, err := Open(db, func() time.Time { return epoch })
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Checkpoint(context.Background(), c.ID)
	if err != nil || got.Sequence != 10 || len(got.Tuples) != 1 || got.Tuples[0] != a {
		t.Fatal("ledger lost checkpoint", got, err)
	}
	got, err = s.Checkpoint(context.Background(), unknown.ID)
	if err != nil || got.Coverage != "unknown" || got.Reason != "inventory_unsupported" || len(got.Tuples) != 0 {
		t.Fatal("unknown checkpoint", got, err)
	}
	if got, err = s.Checkpoint(context.Background(), 0); err == nil && got.Coverage == "known" {
		t.Fatal("absent checkpoint is known")
	}
	// A different host incarnation never extends the old rows.
	put(t, s, checkpoint("inv-2", 3, a))
	if rows := ledgerRows(t, db); len(rows) != 2 {
		t.Fatal(rows)
	}
}

func TestLedgerStaleOrConflictingSequenceIsUnknown(t *testing.T) {
	s, _, _ := openTestStore(t)
	a := tuple("a", "ref-A", 5, "active")
	first := put(t, s, checkpoint("inv", 30, a))
	if again := put(t, s, checkpoint("inv", 30, a)); again.ID != first.ID || again.Coverage != "known" {
		t.Fatal("identical re-read not idempotent", again)
	}
	if stale := put(t, s, checkpoint("inv", 20, a)); stale.Coverage != "unknown" || stale.Reason != "stale_inventory_sequence" {
		t.Fatal("stale checkpoint accepted", stale)
	}
	if conflict := put(t, s, checkpoint("inv", 30, a, tuple("b", "ref-A", 6, "active"))); conflict.Coverage != "unknown" || conflict.Reason != "conflicting_inventory_sequence" {
		t.Fatal("conflicting equal sequence accepted", conflict)
	}
}

func TestLedgerSamplesRoundTripNewestFirstAndBounded(t *testing.T) {
	s, _, _ := openTestStore(t)
	f := newCarryFixture()
	ctx := context.Background()
	for i, sample := range []Sample{f.in.S1, f.in.S2} {
		sample.Before, sample.After = int64(i+1), int64(i+2)
		if _, err := s.PutSample(ctx, sample); err != nil {
			t.Fatal(err)
		}
	}
	other := f.in.S1
	other.Account = "other"
	if _, err := s.PutSample(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, err := s.Samples(ctx, "acct", 10)
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	if !got[0].At.Equal(f.in.S2.At) || got[0].ID == 0 || got[0].Before != 2 || !reflect.DeepEqual(got[0].A, f.in.S2.A) || !got[0].A[0].Cut.At.Equal(f.in.S2.A[0].Cut.At) {
		t.Fatal("sample round trip", got[0])
	}
	old := maxSamples
	maxSamples = 3
	defer func() { maxSamples = old }()
	for i := 0; i < 5; i++ {
		x := f.in.S2
		x.At = x.At.Add(time.Duration(i+1) * time.Hour)
		if _, err = s.PutSample(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.Samples(ctx, "acct", 100)
	if err != nil || len(all) > 3 {
		t.Fatal("sample ledger unbounded", len(all), err)
	}
}
