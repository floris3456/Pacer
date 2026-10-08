package historyreports

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

var forwardAt = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

func forwardInt(n int64) *int64 { return &n }
func forwardNative(t *testing.T, id string, at time.Time) measurement.Record {
	t.Helper()
	// Real existing converter, independent PascalCase/snake_case wire fixture.
	wire := map[string]any{"RequestID": id, "LifecycleRequestID": "logical-shared", "Provider": "claude", "AuthID": "synthetic-auth", "AuthIndex": "index", "Model": "claude-opus-5-5", "Alias": "friendly", "ResponseServiceTier": "default", "RequestedAt": at, "Latency": int64(time.Second), "Failed": false, "AccountingVersion": 2,
		"CachePolicy":    map[string]any{"dispatched": true, "upstream_started_at": at},
		"TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 1000000, "unclassified_tokens": 0, "input": map[string]any{"total_tokens": 1000000, "uncached_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 1000000}, "output": map[string]any{"total_tokens": 0, "non_reasoning_tokens": 0, "reasoning_tokens": 0}},
		"Detail":         map[string]any{"InputTokens": 0, "CachedTokens": 0, "CacheReadTokens": 0, "CacheCreationTokens": 1000000, "OutputTokens": 0, "ReasoningTokens": 0, "TotalTokens": 1000000, "CacheCreation": map[string]any{"quality": "complete", "total_tokens": 1000000, "ephemeral_5m_input_tokens": 250000, "ephemeral_1h_input_tokens": 750000, "unknown_tokens": 0}}}
	raw, _ := json.Marshal(wire)
	r, err := measurement.FromNative(raw, measurement.NativeCredential{Provider: "claude", AccountID: "native-account", CredentialRef: "credential", AuthID: "synthetic-auth", AuthIndex: "index"}, "producer")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func forwardFeed(t *testing.T) (*measurement.Store, *sql.DB) {
	t.Helper()
	db, e := sql.Open("sqlite3", filepath.Join(t.TempDir(), "native.sqlite")+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s, e := measurement.Open(context.Background(), db, measurement.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return s, db
}
func forwardLedger(t *testing.T) (*ForwardStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forward.sqlite")
	s, e := OpenForward(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}
func forwardPut(t *testing.T, s *measurement.Store, r measurement.Record) {
	t.Helper()
	if _, e := s.PutRecord(context.Background(), r); e != nil {
		t.Fatal(e)
	}
}
func forwardAdvance(t *testing.T, s *ForwardStore, feed *measurement.Store) measurement.ReportFeedHead {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		done, e := feed.BootstrapReportFeed(ctx, measurement.ReportBudget{Rows: 3})
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	h, e := feed.ReportFeedHead(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for {
		done, e := s.AdvanceForward(ctx, feed, h, measurement.ReportBudget{Rows: 3})
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	return h
}
func forwardBook(t *testing.T, s *ForwardStore) string {
	t.Helper()
	book := ForwardBook{ID: "synthetic-opus-v1", Book: ForwardComparisonBook{ID: "synthetic-opus-v1", Policy: "profile-api-value-v3", Prices: map[string]ForwardModelPrice{"claude-opus-5-5": {Prompt: 4, Completion: 20, CacheRead: .2}}}, Qualifications: []ForwardPriceQualification{{SourceID: "producer", Provider: "claude", Model: "claude-opus-5-5", AccountID: "native-account", CredentialRef: "credential"}}}
	d, e := s.RegisterForwardBook(context.Background(), book)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func forwardSeal(t *testing.T, s *ForwardStore, h measurement.ReportFeedHead, book string, revision int64) ForwardPin {
	t.Helper()
	p, e := s.SealForward(context.Background(), ForwardSeal{Head: h, BookDigest: book, ProfileRevision: revision, At: forwardAt.Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestForwardNativeLibraryReplayProfilesAndEnrichment(t *testing.T) {
	ctx := context.Background()
	feed, _ := forwardFeed(t)
	s, path := forwardLedger(t)
	book := forwardBook(t, s)
	r := forwardNative(t, "A", forwardAt)
	r.Usage.CacheCreate5m = nil
	r.Usage.CacheCreate1h = nil
	forwardPut(t, feed, r)
	head := forwardAdvance(t, s, feed)
	ref := ForwardAccountRef(r.SourceID, r.Attribution.Provider, r.Attribution.AccountID)
	profile := ForwardProfile{ID: "p1", Name: "Dated", Provider: "claude", Memberships: []ForwardMembership{{Account: ref, Start: forwardAt.UnixMilli(), End: forwardAt.Add(time.Hour).UnixMilli()}}}
	rev, e := s.EditProfiles(ctx, 0, ForwardProfileEdit{Profile: profile})
	if e != nil {
		t.Fatal(e)
	}
	p1 := forwardSeal(t, s, head, book, rev)
	a, e := s.ForwardReport(ctx, p1)
	if e != nil {
		t.Fatal(e)
	}
	if a.Totals.UniqueAttempts == nil || *a.Totals.UniqueAttempts != 1 || a.Totals.SelectionValue == nil || a.Totals.SelectionValue.Low != 5e9 || a.Totals.SelectionValue.High != 8e9 || a.Profiles["p1"].Rows != 1 || a.Capacity != "unavailable" {
		t.Fatalf("bad first report %+v", a)
	}
	var beforeJSON, beforeCSV bytes.Buffer
	if e = s.ExportForwardJSON(ctx, p1, &beforeJSON); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ExportForwardCSV(ctx, p1, 25, "", &beforeCSV); e != nil {
		t.Fatal(e)
	}
	r.Revision = 2
	r.Usage.CacheCreate5m = forwardInt(250000)
	r.Usage.CacheCreate1h = forwardInt(750000)
	forwardPut(t, feed, r)
	// Same lifecycle is a real separate retry, not an enrichment/delivery duplicate.
	retry := forwardNative(t, "B", forwardAt.Add(time.Nanosecond))
	forwardPut(t, feed, retry)
	forwardPut(t, feed, retry)
	head = forwardAdvance(t, s, feed)
	p2 := forwardSeal(t, s, head, book, rev)
	b, e := s.ForwardReport(ctx, p2)
	if e != nil {
		t.Fatal(e)
	}
	if *b.Totals.UniqueAttempts != 2 || b.Totals.SelectionValue == nil || b.Totals.SelectionValue.Low != 145e8 || b.Totals.SelectionValue.High != 145e8 {
		t.Fatalf("revision/retry %+v", b.Totals)
	}
	page, e := s.ForwardRequests(ctx, p2, 1, "")
	if e != nil || page.Next == "" {
		t.Fatal(page, e)
	}
	if _, e = s.ForwardRequests(ctx, p1, 1, page.Next); !errors.Is(e, ErrCursor) {
		t.Fatal("cross-pin cursor accepted", e)
	}
	if _, e = s.ForwardRequests(ctx, p2, 2, page.Next); !errors.Is(e, ErrCursor) {
		t.Fatal("cross-limit cursor accepted", e)
	}
	if _, e = s.EditProfiles(ctx, 0, ForwardProfileEdit{ArchiveID: "p1"}); !errors.Is(e, ErrForwardConflict) {
		t.Fatal(e)
	}
	profile.ID = "p2"
	rev, e = s.EditProfiles(ctx, rev, ForwardProfileEdit{Profile: profile})
	if e != nil {
		t.Fatal(e)
	}
	p3 := forwardSeal(t, s, head, book, rev)
	c, e := s.ForwardReport(ctx, p3)
	if e != nil || c.Totals.Rows != 2 || c.Profiles["p1"].Rows != 2 || c.Profiles["p2"].Rows != 2 {
		t.Fatal(c, e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = OpenForward(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var afterJSON, afterCSV bytes.Buffer
	if e = s.ExportForwardJSON(ctx, p1, &afterJSON); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ExportForwardCSV(ctx, p1, 25, "", &afterCSV); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(beforeJSON.Bytes(), afterJSON.Bytes()) || !bytes.Equal(beforeCSV.Bytes(), afterCSV.Bytes()) {
		t.Fatal("old export changed after enrichment/profile edit/reopen")
	}
	next, e := s.ForwardRequests(ctx, p2, 1, page.Next)
	if e != nil || len(next.Rows) != 1 || next.Next != "" {
		t.Fatal(next, e)
	}
	proof, e := s.ForwardEvidence(ctx, p1, "native-event:"+r.EventID)
	if e != nil || len(proof.Origins) != 1 || proof.Origins[0].Revision != 1 || proof.Value.High != 8e9 {
		t.Fatal(proof, e)
	}
	endPin, e := s.PinForward(ctx, p2.Generation, ForwardFilter{From: forwardAt.Add(time.Nanosecond), To: forwardAt.Add(2 * time.Nanosecond)})
	if e != nil {
		t.Fatal(e)
	}
	end, e := s.ForwardReport(ctx, endPin)
	if e != nil || end.Totals.Rows != 1 {
		t.Fatal(end, e)
	}
}

func TestForwardUnknownsAndOriginalValueDomain(t *testing.T) {
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	bookID := forwardBook(t, s)
	ctx := context.Background()
	cases := []struct {
		name   string
		edit   func(*measurement.Record)
		reason string
	}{
		{"unknown-model", func(r *measurement.Record) { r.Attribution.Model = "unknown" }, "missing_price_qualification"},
		{"unsupported", func(r *measurement.Record) { r.Attribution.Provider = "antigravity" }, "unsupported_capture_provider"},
		{"partial", func(r *measurement.Record) { r.Usage.Quality = "partial" }, "incomplete_accounting"},
		{"counter", func(r *measurement.Record) { r.Usage.Total = forwardInt(3) }, "invalid_counters"},
		{"version", func(r *measurement.Record) { r.Usage.AccountingVersion = "future" }, "unsupported_accounting"},
		{"tier", func(r *measurement.Record) { r.Attribution.ServiceTier = "auto" }, "charged_tier_unknown"},
	}
	for _, tt := range cases {
		r := forwardNative(t, tt.name, forwardAt)
		tt.edit(&r)
		forwardPut(t, feed, r)
	}
	head := forwardAdvance(t, s, feed)
	pin := forwardSeal(t, s, head, bookID, 0)
	report, e := s.ForwardReport(ctx, pin)
	if e != nil {
		t.Fatal(e)
	}
	if report.Totals.Rows != int64(len(cases)) || report.Totals.SelectionValue != nil || report.Totals.ValuedCount != 0 {
		t.Fatal(report.Totals)
	}
	for _, tt := range cases {
		if report.Totals.Reasons[tt.reason] == 0 {
			t.Fatal("lost explicit unknown", tt.reason)
		}
	}
	var book ForwardBook
	if e = s.object(ctx, "book", bookID, &book); e != nil {
		t.Fatal(e)
	}
	for _, split := range []bool{false, true} {
		r := forwardNative(t, fmt.Sprint(split), forwardAt)
		if !split {
			r.Usage.CacheCreate5m = nil
			r.Usage.CacheCreate1h = nil
		}
		entry := measurement.ReportFeedEntry{StoredRecord: measurement.StoredRecord{Record: r, ReceivedAt: forwardAt}}
		f := convertForward(entry)
		v, issues := valueForward(f, book)
		if v == nil || len(issues) != 0 {
			t.Fatal(v, issues)
		}
		original := cpamp.ForwardValue(f.Event, book.Book)
		if !reflect.DeepEqual(*v, original) {
			t.Fatal("original valuation diverged")
		}
		native := measurement.ValueRecord(r, measurement.DefaultPriceBook())
		if v.Low != native.LowNanoUSD || v.High != native.HighNanoUSD {
			t.Fatal(v, native)
		}
	}
	bad := forwardNative(t, "overflow", forwardAt)
	bad.Usage.NonCacheInput = math.MaxInt64
	f := convertForward(measurement.ReportFeedEntry{StoredRecord: measurement.StoredRecord{Record: bad}})
	v, _ := valueForward(f, book)
	if v != nil {
		t.Fatal("overflow priced")
	}
	acc := ForwardValue{Low: math.MaxInt64}
	if e = addForwardValue(&acc, ForwardValue{Low: 1}); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
	// Explicit zero category rates are free; absent zero rates are missing.
	f = convertForward(measurement.ReportFeedEntry{StoredRecord: measurement.StoredRecord{Record: forwardNative(t, "free", forwardAt)}})
	price := book.Book.Prices[f.Event.Model]
	price.Prompt = 0
	price.PromptConfigured = true
	book.Book.Prices[f.Event.Model] = price
	v, _ = valueForward(f, book)
	if v == nil || v.High != 0 {
		t.Fatal(v)
	}
	price.PromptConfigured = false
	book.Book.Prices[f.Event.Model] = price
	v, _ = valueForward(f, book)
	if v != nil {
		t.Fatal("missing price became free")
	}
}

func TestForwardConcurrentOldPinAndDestinationFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := forwardLedger(t)
	feed, _ := forwardFeed(t)
	book := forwardBook(t, s)
	forwardPut(t, feed, forwardNative(t, "first", forwardAt))
	h := forwardAdvance(t, s, feed)
	pin := forwardSeal(t, s, h, book, 0)
	var want bytes.Buffer
	if err := s.ExportForwardJSON(ctx, pin, &want); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_projection BEFORE INSERT ON forward_native BEGIN SELECT RAISE(ABORT,'synthetic projection failure'); END`); err != nil {
		t.Fatal(err)
	}
	r := forwardNative(t, "second", forwardAt)
	forwardPut(t, feed, r)
	newHead, err := feed.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdvanceForward(ctx, feed, newHead, measurement.ReportBudget{}); err == nil {
		t.Fatal("projection fault suppressed")
	}
	stored, err := feed.Record(ctx, r.EventID, 1)
	if err != nil || stored.EventID != r.EventID {
		t.Fatal(stored, err)
	}
	status, err := s.Status(ctx)
	if err != nil || status.Through != h.Through || status.Latest != pin.Generation {
		t.Fatal(status, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_projection`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				var b bytes.Buffer
				if err := s.ExportForwardJSON(ctx, pin, &b); err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(want.Bytes(), b.Bytes()) {
					errs <- fmt.Errorf("old pin changed")
					return
				}
			}
		}()
	}
	newHead = forwardAdvance(t, s, feed)
	forwardSeal(t, s, newHead, book, 0)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.ForwardReport(c, pin); err == nil {
		t.Fatal("cancellation ignored")
	}
	if strings.Contains(want.String(), "synthetic-auth") {
		t.Fatal("private transport leaked")
	}
}
