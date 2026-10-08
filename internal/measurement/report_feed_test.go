package measurement

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReportFeedAtomicFailureAndRevisions(t *testing.T) {
	s, db := fixture(t)
	r := record("feed-attempt", base, 1)
	if _, err := db.Exec(`CREATE TRIGGER reject_feed BEFORE INSERT ON measurement_report_feed BEGIN SELECT RAISE(ABORT,'synthetic feed failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRecord(ctx, r); err == nil {
		t.Fatal("feed failure acknowledged")
	}
	for _, table := range []string{"measurement_records", "measurement_record_revisions", "measurement_report_feed"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
	if _, err := db.Exec(`DROP TRIGGER reject_feed`); err != nil {
		t.Fatal(err)
	}
	put(t, s, r)
	r.Revision = 2
	r.Usage.CacheCreate = 9
	put(t, s, r)
	r.Revision = 1
	r.Usage.CacheCreate = 0
	result, err := s.PutRecord(ctx, r)
	if err != nil || !result.Duplicate {
		t.Fatal(result, err)
	}
	r.Usage.Output = 9
	if _, err = s.PutRecord(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err != nil || !done {
		t.Fatal(done, err)
	}
	h, err := s.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forged := h
	forged.Through = h.BootstrapThrough - 1
	if _, err = s.ReadReportFeed(ctx, forged, 0, ReportBudget{}); !errors.Is(err, ErrConflict) {
		t.Fatal("pre-bootstrap head accepted", err)
	}
	p, err := s.ReadReportFeed(ctx, h, 0, ReportBudget{Rows: 1})
	if err != nil || p.Done || len(p.Entries) != 1 || len(p.Entries[0].RawUsage) != 0 {
		t.Fatal(p, err)
	}
	p, err = s.ReadReportFeed(ctx, h, p.Next, ReportBudget{})
	if err != nil || !p.Done || len(p.Entries) != 1 || p.Entries[0].Revision != 2 {
		t.Fatal(p, err)
	}
	if p.Entries[0].RawDigest != digest(r.RawUsage) {
		t.Fatal("raw evidence digest lost")
	}
}

func TestReportFeedFiniteBootstrapConcurrentAndReopen(t *testing.T) {
	s, db := fixture(t)
	// Simulate an existing v2 database before the additive extension was installed.
	if _, err := db.Exec(`DROP TRIGGER measurement_report_revision_insert; DROP TABLE measurement_report_feed; DROP TABLE measurement_report_meta`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "d", "z"} {
		put(t, s, record(id, base, 1))
	}
	var err error
	s, err = Open(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.BootstrapReportFeed(ctx, ReportBudget{Rows: 1})
	if err != nil || done {
		t.Fatal(done, err)
	}
	put(t, s, record("a", base, 1))
	put(t, s, record("zz", base, 1))
	r := record("z", base, 1)
	r.Revision = 2
	r.Usage.Output = 17
	put(t, s, r)
	s, err = Open(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8 && !done; i++ {
		done, err = s.BootstrapReportFeed(ctx, ReportBudget{Rows: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("bootstrap did not stop at finite start key")
	}
	h, err := s.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadReportFeed(ctx, h, 0, ReportBudget{})
	if err != nil || !page.Done || len(page.Entries) != 6 {
		t.Fatal(page, err)
	}
	var r1, r2 int64
	for _, e := range page.Entries {
		if e.EventID == "z" {
			if e.Revision == 1 {
				r1 = e.Sequence
			} else {
				r2 = e.Sequence
			}
		}
	}
	if r1 <= r2 {
		t.Fatal("fixture must backfill r1 after live r2", r1, r2)
	}
	if _, err = s.ReadReportFeed(ctx, h, 0, ReportBudget{Bytes: 1}); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	c, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if _, err = s.ReadReportFeed(c, h, 0, ReportBudget{}); err == nil {
		t.Fatal("deadline ignored")
	}
}
