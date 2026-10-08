package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
var ctx = context.Background()

func integer(n int64) *int64 { return &n }

func openTest(t *testing.T, path string) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	s, err := Open(ctx, db, Options{Clock: func() time.Time { return base.Add(40 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterPriceBook(ctx, DefaultPriceBook()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func fixture(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	s, db := openTest(t, filepath.Join(t.TempDir(), "usage.sqlite"))
	if err := s.StartDelivery(ctx, "run-1", base.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "run-1", base.Add(35*24*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	return s, db
}

func record(id string, at time.Time, dollars int64) Record {
	return Record{EventID: id, SourceID: "host-a", Revision: 1, RequestID: "request-" + id, AttemptID: "attempt-" + id, StreamID: "stream-" + id,
		Attribution: Attribution{Provider: "claude", AccountID: "a", CallerID: "caller", KeyRef: "sha256:key-ref", InstallationID: "installation", SessionID: "chat", ProviderRoute: "claude-main", Model: "claude-opus-5-5"},
		Policy:      PolicySnapshot{ContextEpoch: "epoch-1", Revision: 7, SelectedTTL: TTL5m, EffectiveTTL: TTL5m, RequestStartedAt: at}, Activity: Activity{PrimaryPrefix: true, ProtectionTTL: TTL5m}, Kind: "primary",
		StartedAt: at, EndedAt: at.Add(time.Second), Final: true, Success: true, Usage: Usage{AccountingVersion: "normalized-v1", Quality: "complete", NonCacheInput: dollars * 250000}, RawUsage: json.RawMessage("{\n  \"input_tokens\": 12\n}")}
}

func put(t *testing.T, s *Store, r Record) {
	t.Helper()
	if _, err := s.PutRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
}
func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("got %.12f want %.12f", got, want)
	}
}
func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func query(at time.Time) Query {
	return Query{Provider: "claude", AccountID: "a", Scope: "all", BookID: DefaultPriceBook().ID, Since: base, AsOf: at}
}
func measure(t *testing.T, s *Store, q Query) Report {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r, err := s.Measure(c, q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func observation(t *testing.T, s *Store, id, cycle string, duration time.Duration, at time.Time, pct float64) Observation {
	t.Helper()
	o := Observation{ID: id, Provider: "claude", AccountID: "a", Scope: "all", CycleID: cycle, Duration: duration, At: at, UsedPercent: pct, Source: "response-header", Usable: true}
	if err := s.PutObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	return o
}

func closeCycle(t *testing.T, s *Store, o Observation, at time.Time) {
	t.Helper()
	if err := s.CloseCycle(ctx, Closure{CycleRef: o.ref(), At: at, Source: "observed-new-cycle"}); err != nil {
		t.Fatal(err)
	}
}

func TestValueReportedMixedWritesAndUnknownRemainder(t *testing.T) {
	r := record("mixed", base, 0)
	r.Policy.SelectedTTL = TTL1h
	r.Policy.EffectiveTTL = TTL1h
	r.Usage.CacheCreate = 1000000
	r.Usage.CacheCreate5m = integer(250000)
	r.Usage.CacheCreate1h = integer(750000)
	v := ValueRecord(r, DefaultPriceBook())
	near(t, v.LowUSD(), 7.25)
	near(t, v.HighUSD(), 7.25)
	if !v.Exact() || v.UnknownWrites != 0 || v.InferredWrites != 0 {
		t.Fatal(v)
	}
	r.Usage.CacheCreate1h = nil
	v = ValueRecord(r, DefaultPriceBook())
	near(t, v.LowUSD(), 5)
	near(t, v.HighUSD(), 7.25)
	if v.Exact() || v.UnknownWrites != 750000 {
		t.Fatal(v)
	}
	r.Usage.CacheCreate5m = nil
	v = ValueRecord(r, DefaultPriceBook())
	near(t, v.LowUSD(), 5)
	near(t, v.HighUSD(), 8)
	if v.InferredWrites != 0 {
		t.Fatal("selected/effective TTL is not complete write evidence", v)
	}
	r.Policy.AllWritesCovered = true
	v = ValueRecord(r, DefaultPriceBook())
	near(t, v.LowUSD(), 8)
	if !v.Exact() || v.InferredWrites != 1000000 || !has(v.Issues, "policy_inferred_writes") {
		t.Fatal(v)
	}
	r.Usage.CacheCreate5m = integer(250000)
	v = ValueRecord(r, DefaultPriceBook())
	if v.Exact() || !has(v.Issues, "wire_usage_mismatch") {
		t.Fatal(v)
	}
}

func TestValueInvalidUnknownModelsTiersAndReasoning(t *testing.T) {
	r := record("value", base, 0)
	r.Usage.Output = 1000000
	r.Usage.Reasoning = 400000
	v := ValueRecord(r, DefaultPriceBook())
	near(t, v.LowUSD(), 20)
	r.Usage.Total = integer(1400000)
	if ValueRecord(r, DefaultPriceBook()).InvalidCounters != 1 {
		t.Fatal("double reasoning accepted")
	}
	r.Usage.Total = nil
	r.Usage.CacheCreate = 10
	r.Usage.CacheCreate5m = integer(11)
	if ValueRecord(r, DefaultPriceBook()).InvalidCounters != 1 {
		t.Fatal("overfull split accepted")
	}
	r.Usage.CacheCreate5m = nil
	r.Usage.CacheCreate = 0
	r.Attribution.Model = "claude-haiku-5-5"
	v = ValueRecord(r, DefaultPriceBook())
	if v.Exact() || v.MissingPrices != 1 {
		t.Fatal("unverified model was guessed", v)
	}
	r.Attribution.Model = "claude-opus-5-5"
	r.Attribution.ServiceTier = "priority"
	if ValueRecord(r, DefaultPriceBook()).MissingPrices != 1 {
		t.Fatal("unpriced tier inherited standard")
	}
	r.Attribution.ServiceTier = "standard"
	r.Usage.Quality = "partial"
	if ValueRecord(r, DefaultPriceBook()).Exact() {
		t.Fatal("partial counters appear exact")
	}
	r.Usage.Quality = "complete"
	r.Usage.NonCacheInput = math.MaxInt64
	r.Usage.CacheRead = 1
	if ValueRecord(r, DefaultPriceBook()).InvalidCounters != 1 {
		t.Fatal("overflow accepted")
	}
}

func TestPinnedPriceBooksAndExplicitContextTiers(t *testing.T) {
	s, _ := fixture(t)
	b := DefaultPriceBook()
	b.Prices[0].Rates.Input = number(99)
	if !errors.Is(s.RegisterPriceBook(ctx, b), ErrConflict) {
		t.Fatal("book mutated in place")
	}
	b.ID = "explicit-context-v1"
	b.Prices[0].MaxInput = 1000000
	p := b.Prices[0]
	p.MinInput = 1000000
	p.MaxInput = 0
	p.Rates.Input = number(8)
	b.Prices = append(b.Prices, p)
	if err := s.RegisterPriceBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	r := record("context", base, 4)
	near(t, ValueRecord(r, b).LowUSD(), 8)
	b.Prices[1].MinInput = 999999
	if !errors.Is(s.RegisterPriceBook(ctx, b), ErrInvalid) {
		t.Fatal("overlapping tiers accepted")
	}
}

func TestSQLiteDedupRetryRevisionsAndImmutableLatePolicy(t *testing.T) {
	s, db := fixture(t)
	r := record("physical-1", base.Add(time.Minute), 0)
	r.RequestID = "logical-one"
	r.Usage.CacheCreate = 1000000
	var inserted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.PutRecord(ctx, r)
			if err != nil {
				t.Error(err)
			}
			if result.Inserted {
				inserted.Add(1)
			}
		}()
	}
	wg.Wait()
	if inserted.Load() != 1 {
		t.Fatal(inserted.Load())
	}
	got, err := s.Record(ctx, r.EventID, 0)
	if err != nil || string(got.RawUsage) != string(r.RawUsage) {
		t.Fatal("raw evidence changed", got, err)
	}
	if got.Usage.CacheCreate5m != nil {
		t.Fatal("missing split became zero")
	}
	retry := r
	retry.EventID = "physical-2"
	retry.AttemptID = "attempt-2"
	put(t, s, retry)
	bad := r
	bad.EventID = "different-event-same-attempt"
	if _, err = s.PutRecord(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatal("physical attempt duplicated", err)
	}
	late := r
	late.Revision = 2
	late.Usage.CacheCreate5m = integer(250000)
	late.Usage.CacheCreate1h = integer(750000)
	late.RawUsage = json.RawMessage(`{"cache_creation":{"ephemeral_5m_input_tokens":250000,"ephemeral_1h_input_tokens":750000}}`)
	put(t, s, late)
	late.Usage.CacheCreate5m = integer(999999) // Caller mutation cannot alter stored evidence.
	got, err = s.Record(ctx, r.EventID, 0)
	if err != nil || got.Revision != 2 || counter(got.Usage.CacheCreate5m) != 250000 || got.Policy.ContextEpoch != "epoch-1" || got.Policy.Revision != 7 {
		t.Fatal(got, err)
	}
	old, err := s.Record(ctx, r.EventID, 1)
	if err != nil || old.Usage.CacheCreate5m != nil || string(old.RawUsage) != string(r.RawUsage) {
		t.Fatal(old, err)
	}
	if result, err := s.PutRecord(ctx, r); err != nil || !result.Duplicate {
		t.Fatal("old delivery not idempotent", result, err)
	}
	bad = got.Record
	bad.Revision = 3
	bad.Policy.ContextEpoch = "current-epoch-2"
	if _, err = s.PutRecord(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatal("late usage rebound to current policy", err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM measurement_records`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM measurement_record_revisions`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
}

func TestRestartRetainsRecordsAndMarksNativeDeliveryGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	s, db := openTest(t, path)
	if err := s.StartDelivery(ctx, "before", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "before", base.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	r := record("restart-record", base.Add(time.Minute), 4)
	put(t, s, r)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, db = openTest(t, path)
	if err := s.StartDelivery(ctx, "after", base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "after", base.Add(3*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PutRecord(ctx, r); err != nil || !result.Duplicate {
		t.Fatal(result, err)
	}
	report := measure(t, s, query(base.Add(3*time.Hour)))
	if report.Coverage.Observed || !has(report.Coverage.Reasons, "delivery_gap:observer_restart_or_reload") || report.Coverage.Delivery != "native-in-memory-best-effort" {
		t.Fatal(report.Coverage)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM measurement_record_revisions`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	q := query(base.Add(30 * time.Minute))
	q.Since = base.Add(-time.Minute)
	if measure(t, s, q).Coverage.Observed {
		t.Fatal("invented history before observer start")
	}
}

func TestReadOnlySingleConnectionSnapshotLimitAndSchemaVersion(t *testing.T) {
	s, db := fixture(t)
	put(t, s, record("read-only-1", base.Add(time.Minute), 4))
	put(t, s, record("read-only-2", base.Add(2*time.Minute), 4))
	o := observation(t, s, "o1", "cycle", 5*time.Hour, base, 0)
	observation(t, s, "o2", "cycle", 5*time.Hour, base.Add(time.Hour), 10)
	closeCycle(t, s, o, base.Add(5*time.Hour))
	if _, err := db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	report := measure(t, s, query(base.Add(6*time.Hour)))
	if !report.FiveHour.Known {
		t.Fatal(report.Windows)
	}
	q := query(base.Add(6 * time.Hour))
	q.MaxRecords = 1
	if _, err := s.Measure(ctx, q); !errors.Is(err, ErrLimit) {
		t.Fatal("silent truncation", err)
	}
	if _, err := db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE measurement_meta SET version=99`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, db, Options{}); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatal(err)
	}
}

func TestFiveHourLastTwoDivergenceAndWeightedRatio(t *testing.T) {
	for _, test := range []struct {
		second int64
		want   float64
		method string
	}{{8, .4, "lower_of_divergent_last_two"}, {6, .5, "delta_weighted_last_two"}} {
		t.Run(fmt.Sprint(test.second), func(t *testing.T) {
			s, _ := fixture(t)
			for i, cost := range []int64{4, test.second} {
				at := base.Add(time.Duration(i) * 5 * time.Hour)
				cycle := fmt.Sprint(i)
				a := observation(t, s, "start-"+cycle, cycle, 5*time.Hour, at, 10)
				observation(t, s, "end-"+cycle, cycle, 5*time.Hour, at.Add(time.Hour), 20)
				put(t, s, record("cost-"+cycle, at.Add(time.Minute), cost))
				closeCycle(t, s, a, at.Add(5*time.Hour))
			}
			r := measure(t, s, query(base.Add(11*time.Hour)))
			if !r.FiveHour.Known || r.FiveHour.Method != test.method || len(r.FiveHour.CycleIDs) != 2 {
				t.Fatal(r.FiveHour, r.Windows)
			}
			near(t, r.FiveHour.USDPerPercent, test.want)
		})
	}
}

func TestWeeklyCurrentTenPointsElseLastCompleted(t *testing.T) {
	s, _ := fixture(t)
	week := 7 * 24 * time.Hour
	o := observation(t, s, "old-a", "old", week, base, 20)
	observation(t, s, "old-b", "old", week, base.Add(time.Hour), 30)
	put(t, s, record("old-value", base.Add(time.Minute), 30))
	closeCycle(t, s, o, base.Add(week))
	at := base.Add(week)
	observation(t, s, "current-a", "current", week, at, 0)
	observation(t, s, "current-b", "current", week, at.Add(time.Hour), 9)
	put(t, s, record("current-value", at.Add(time.Minute), 40))
	r := measure(t, s, query(at.Add(90*time.Minute)))
	near(t, r.SevenDay.USDPerPercent, 3)
	if r.SevenDay.Method != "last_completed_window" {
		t.Fatal(r.SevenDay)
	}
	observation(t, s, "current-c", "current", week, at.Add(2*time.Hour), 10)
	put(t, s, record("current-rounding", at.Add(91*time.Minute), 0))
	r = measure(t, s, query(at.Add(3*time.Hour)))
	near(t, r.SevenDay.USDPerPercent, 4)
	if r.SevenDay.Method != "current_window_ten_points" {
		t.Fatal(r.SevenDay)
	}
}

func TestIntervalsRejectOutsideUseUnknownMixBoundariesAndGaps(t *testing.T) {
	for _, test := range []string{"outside", "unknown-mix", "crossing", "gap", "small-delta", "unknown-model", "incomplete"} {
		t.Run(test, func(t *testing.T) {
			s, _ := fixture(t)
			o := observation(t, s, "start", "cycle", 5*time.Hour, base, 0)
			delta := 10.0
			if test == "small-delta" {
				delta = 9.99
			}
			observation(t, s, "mid", "cycle", 5*time.Hour, base.Add(time.Hour), delta)
			r := record("usage", base.Add(time.Minute), 4)
			switch test {
			case "outside":
				observation(t, s, "outside", "cycle", 5*time.Hour, base.Add(2*time.Hour), delta+1)
			case "unknown-mix":
				r.Usage.CacheCreate = 1000000
			case "crossing":
				r.StartedAt = base.Add(-time.Minute)
				r.Policy.RequestStartedAt = r.StartedAt
			case "gap":
				if err := s.PutGap(ctx, Gap{ID: "drop", From: base.Add(5 * time.Minute), To: base.Add(6 * time.Minute), AccountID: "a", Reason: "callback_dropped"}); err != nil {
					t.Fatal(err)
				}
			case "unknown-model":
				r.Attribution.Model = "no-pinned-price"
			case "incomplete":
				r.Usage.Quality = "unknown"
			}
			put(t, s, r)
			closeCycle(t, s, o, base.Add(5*time.Hour))
			report := measure(t, s, query(base.Add(6*time.Hour)))
			if report.FiveHour.Known || len(report.Windows) != 1 || report.Windows[0].Eligible {
				t.Fatal(report.FiveHour, report.Windows)
			}
			if test == "outside" && !has(report.Windows[0].Reasons, "outside_usage_without_cpa_requests") {
				t.Fatal(report.Windows)
			}
			if test == "crossing" {
				near(t, report.Windows[0].Value.LowUSD(), 0)
				near(t, report.Windows[0].Value.HighUSD(), 4)
			}
		})
	}
}

func TestLateFinalUsageRecomputesWithoutDoubleCount(t *testing.T) {
	s, _ := fixture(t)
	o := observation(t, s, "before", "cycle", 5*time.Hour, base, 0)
	observation(t, s, "after", "cycle", 5*time.Hour, base.Add(time.Hour), 10)
	closeCycle(t, s, o, base.Add(5*time.Hour))
	r := record("late", base.Add(time.Minute), 0)
	r.Final = false
	r.Success = false
	r.EndedAt = time.Time{}
	r.Usage.Quality = "partial"
	put(t, s, r)
	before := measure(t, s, query(base.Add(6*time.Hour)))
	if before.FiveHour.Known {
		t.Fatal(before.FiveHour)
	}
	r.Revision = 2
	r.Final = true
	r.Success = true
	r.EndedAt = r.StartedAt.Add(time.Minute)
	r.Usage.Quality = "complete"
	r.Usage.CacheCreate = 1000000
	r.Usage.CacheCreate5m = integer(250000)
	r.Usage.CacheCreate1h = integer(750000)
	put(t, s, r)
	after := measure(t, s, query(base.Add(6*time.Hour)))
	if !after.FiveHour.Known || after.EvidenceID == before.EvidenceID {
		t.Fatal(after.FiveHour)
	}
	near(t, after.FiveHour.USDPerPercent, .725)
	if after.Windows[0].Intervals[0].Requests != 1 {
		t.Fatal("cumulative revisions double counted")
	}
}

func TestConcurrentSQLiteReadersAndWritersAcrossSharedPools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	s, _ := openTest(t, path)
	other, _ := openTest(t, path)
	if err := s.StartDelivery(ctx, "active", base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "active", base.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = other
			}
			r := record("parallel-"+fmt.Sprint(i), base.Add(time.Duration(i)*time.Minute), 1)
			if _, err := store.PutRecord(c, r); err != nil {
				t.Error(err)
				return
			}
			r.Revision = 2
			r.Usage.Output = 100
			if _, err := store.PutRecord(c, r); err != nil {
				t.Error(err)
				return
			}
			if _, err := store.Measure(c, query(base.Add(time.Hour))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 12; i++ {
		r, err := s.Record(c, "parallel-"+fmt.Sprint(i), 0)
		if err != nil || r.Revision != 2 {
			t.Fatal(r, err)
		}
	}
}

func TestScopedQuotaMembershipAndAccountSpecificGap(t *testing.T) {
	s, _ := fixture(t)
	for i, pct := range []float64{0, 10} {
		o := Observation{ID: fmt.Sprint(i), Provider: "claude", AccountID: "a", Scope: "opus", CycleID: "opus-cycle", Duration: 5 * time.Hour, Models: []string{"claude-opus-5-5"}, At: base.Add(time.Duration(i) * time.Hour), UsedPercent: pct, Source: "response-header", Usable: true}
		if err := s.PutObservation(ctx, o); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			closeCycle(t, s, o, base.Add(5*time.Hour))
		}
	}
	put(t, s, record("opus", base.Add(time.Minute), 4))
	unpriced := record("other-model", base.Add(2*time.Minute), 10)
	unpriced.Attribution.Model = "unpriced-model"
	put(t, s, unpriced)
	if err := s.PutGap(ctx, Gap{ID: "other-account", AccountID: "b", From: base, To: base.Add(time.Hour), Reason: "other_account_missing"}); err != nil {
		t.Fatal(err)
	}
	q := query(base.Add(6 * time.Hour))
	q.Scope = "opus"
	r := measure(t, s, q)
	if !r.FiveHour.Known {
		t.Fatal(r.Windows)
	}
	near(t, r.FiveHour.USDPerPercent, .4)
}
