package historyreports

import (
	"context"
	"os"
	"strings"
	"database/sql"
	"encoding/json"
	"fmt"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func TestForward100001BootstrapResumeAndLiveness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	feed, db := forwardFeed(t)
	var sourcePath, dbName string
	var dbSequence int
	if err := db.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&dbSequence, &dbName, &sourcePath); err != nil {
		t.Fatal(err)
	}
	s, path := forwardLedger(t)
	book := forwardBook(t, s)
	// Source-only pre-extension fixture: 100k distinct accepted, immutable v2
	// revisions/heads. Prepared inserts and one synthetic transaction keep setup
	// separate from the startup/publisher resource measurements below.
	if _, err := db.ExecContext(ctx, `DROP TRIGGER measurement_report_revision_insert;DROP TABLE measurement_report_feed;DROP TABLE measurement_report_meta`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	revs, err := tx.PrepareContext(ctx, `INSERT INTO measurement_record_revisions(event_id,revision,received_at,body,raw_usage) VALUES(?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer revs.Close()
	heads, err := tx.PrepareContext(ctx, `INSERT INTO measurement_records(event_id,source_id,attempt_id,provider,account_id,started_at,ended_at,revision,identity,received_at,body,raw_usage) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer heads.Close()
	prototype := forwardNative(t, "seed", forwardAt)
	prototype.RawUsage = nil
	var last measurement.Record
	for i := 0; i < 100000; i++ {
		if err = ctx.Err(); err != nil {
			t.Fatal(err)
		}
		r := prototype
		r.EventID = fmt.Sprintf("bulk-%06d", i)
		r.AttemptID = r.EventID
		r.ExecutionID = r.AttemptID
		r.StartedAt = forwardAt.Add(time.Duration(i / 1000)) // tie-heavy composite keyset
		r.RequestedAt = r.StartedAt
		r.EndedAt = r.StartedAt.Add(time.Second)
		r.Policy.RequestStartedAt = r.StartedAt
		body, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		identity := r
		identity.Revision = 0
		identity.EndedAt = time.Time{}
		identity.Final = false
		identity.Success = false
		identity.Usage = measurement.Usage{}
		identity.CapturedCostNanoUSD = nil
		idBody, _ := json.Marshal(identity)
		if _, e = revs.ExecContext(ctx, r.EventID, 1, forwardAt.UnixNano(), body, []byte{}); e != nil {
			t.Fatal(e)
		}
		if _, e = heads.ExecContext(ctx, r.EventID, r.SourceID, r.AttemptID, r.Attribution.Provider, r.Attribution.AccountID, r.StartedAt.UnixNano(), r.EndedAt.UnixNano(), 1, idBody, forwardAt.UnixNano(), body, []byte{}); e != nil {
			t.Fatal(e)
		}
		last = r
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	feed, err = measurement.Open(ctx, db, measurement.Options{})
	if err != nil {
		t.Fatal(err)
	}
	startup := time.Since(opened)
	h, err := feed.ReportFeedHead(ctx)
	if err != nil || h.Through != 0 || h.BootstrapComplete {
		t.Fatal("startup performed adoption", h, err)
	}
	done, err := feed.BootstrapReportFeed(ctx, measurement.ReportBudget{})
	if err != nil || done {
		t.Fatal(done, err)
	}
	// Live r2 is journaled before the bootstrap reaches old r1 for this key.
	last.Revision = 2
	last.Usage.Output = 2
	last.Usage.Total = forwardInt(1000002)
	if _, err = feed.PutRecord(ctx, last); err != nil {
		t.Fatal(err)
	}
	live := forwardNative(t, "concurrent-live", forwardAt.Add(time.Second))
	type arrival struct {
		err     error
		elapsed time.Duration
	}
	ch := make(chan arrival, 1)
	entered := make(chan struct{})
	go func(source *measurement.Store) {
		close(entered)
		start := time.Now()
		_, e := source.PutRecord(ctx, live)
		ch <- arrival{e, time.Since(start)}
	}(feed)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	steps := 1
	var ingestionLatency time.Duration
	maxBootstrap := time.Duration(0)
	for !done {
		start := time.Now()
		done, err = feed.BootstrapReportFeed(ctx, measurement.ReportBudget{})
		elapsed := time.Since(start)
		if elapsed > maxBootstrap {
			maxBootstrap = elapsed
		}
		if err != nil {
			t.Fatal(err)
		}
		steps++
		if steps == 3 {
			select {
			case v := <-ch:
				ingestionLatency = v.elapsed
				if v.err != nil {
					t.Fatal(v.err)
				}
			case <-ctx.Done():
				t.Fatal("native ingestion blocked by bootstrap", ctx.Err())
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite3", uri(sourcePath, "mode=rw&_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { db.Close() })
			feed, err = measurement.Open(ctx, db, measurement.Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	h, err = feed.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	maxProjection := time.Duration(0)
	projected := 0
	done = false
	for !done {
		start := time.Now()
		done, err = s.AdvanceForward(ctx, feed, h, measurement.ReportBudget{})
		elapsed := time.Since(start)
		if elapsed > maxProjection {
			maxProjection = elapsed
		}
		if err != nil {
			t.Fatal(err)
		}
		projected++
		if projected == 3 {
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenForward(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			st, e := s.Status(ctx)
			if e != nil || st.Latest != "" || st.Through == 0 {
				t.Fatal("partial generation exposed", st, e)
			}
		}
	}
	// Compilation has no source DB connection: an accepted duplicate callback can
	// complete while the 100k report is being compiled, with no extra counted fact.
	accepted := make(chan arrival, 1)
	go func(source *measurement.Store) {
		start := time.Now()
		_, e := source.PutRecord(ctx, live)
		accepted <- arrival{e, time.Since(start)}
	}(feed)
	start := time.Now()
	pin, err := s.SealForward(ctx, ForwardSeal{Head: h, BookDigest: book, At: forwardAt.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	compile := time.Since(start)
	var compileIngestionLatency time.Duration
	select {
	case v := <-accepted:
		compileIngestionLatency = v.elapsed
		if v.err != nil {
			t.Fatal(v.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	report, err := s.ForwardReport(ctx, pin)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.UniqueAttempts == nil || *report.Totals.UniqueAttempts != 100001 || report.Totals.SourceRecords != 100001 || report.Totals.ValuedCount != 100001 || report.Totals.SelectionValue == nil || report.Totals.SelectionValue.Low != 100001*int64(725e7)+40000 {
		t.Fatalf("not an exact 100001 result: %+v", report.Totals)
	}
	if probe := os.Getenv("PACER_FORWARD_FILTER_PROBE"); probe != "" {
		for _, f := range []ForwardFilter{{Domain: "combined", Model: report.Pin.Filter.Model + "no-such-model"}, {Domain: "combined", KeyHash: strings.Repeat("a", 64)}, {Domain: "combined", Profile: "unassigned"}} {
			fp, e := s.PinForward(ctx, pin.Generation, f)
			if e != nil {
				t.Fatal(e)
			}
			start := time.Now()
			if _, e = s.ForwardReport(ctx, fp); e != nil {
				t.Fatal(e)
			}
			t.Logf("filtered forward report %+v: %s", f, time.Since(start))
		}
		start := time.Now()
		if _, e := s.ForwardReport(ctx, pin); e != nil {
			t.Fatal(e)
		}
		t.Logf("default forward report: %s", time.Since(start))
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	var usage syscall.Rusage
	if err = syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	rss := usage.Maxrss
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	c, _, err := s.forwardContext(ctx, pin, false)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.readForwardBatch(ctx, c, pin, 0, "")
	if err != nil || len(page) != 250 {
		t.Fatal("unbounded/report batch", len(page), err)
	}
	used := 0
	for _, r := range page {
		used += len(r.native) + len(r.baseline)
	}
	if used > 2<<20 {
		t.Fatal("report byte ceiling", used)
	}
	t.Logf("synthetic attempts=100001 revisions=100002 bootstrap_batches=%d projection_batches=%d metadata_open=%s max_bootstrap_batch=%s max_projection_batch=%s compile=%s concurrent_ingestion=%s compile_concurrent_duplicate=%s go_heap_inuse=%d process_max_rss_bytes=%d", steps, projected, startup, maxBootstrap, maxProjection, compile, ingestionLatency, compileIngestionLatency, mem.HeapInuse, rss)
}
