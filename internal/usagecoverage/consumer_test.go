package usagecoverage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var ctx = context.Background()
var epoch = time.Date(2026, 10, 8, 0, 0, 0, 123, time.UTC)

type fixture struct {
	c     *Consumer
	store *measurement.Store
	db    *sql.DB
	path  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return openFixture(t, filepath.Join(t.TempDir(), "usage.sqlite"), epoch)
}
func openFixture(t *testing.T, path string, start time.Time) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s, err := measurement.Open(ctx, db, measurement.Options{Clock: func() time.Time { return epoch.Add(24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, s, Options{SourceID: "host-installation", Clock: func() time.Time { return start }})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{c, s, db, path}
}
func tuple(id string) pluginapi.UsageSourceScope {
	return pluginapi.UsageSourceScope{Provider: "claude", AuthID: "PRIVATE-" + id + "@example.invalid.json", AuthIndex: "index-" + id, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
}
func membership(id, account string, tuples ...string) Membership {
	m := Membership{ID: id, Provider: "claude", AccountID: account, Revision: 1, From: epoch, Through: epoch.Add(12 * time.Hour), Verified: true, Complete: true, MappingEvidenceRef: "verified-host-account-claim"}
	for _, v := range tuples {
		m.Members = append(m.Members, Member{Scope: tuple(v), CredentialRef: "credential-" + v})
	}
	return m
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func at(t time.Time) *time.Time { return &t }
func response(id string, seq uint64) pluginapi.HostUsageCheckpointResponse {
	return pluginapi.HostUsageCheckpointResponse{Scope: tuple(id), Coverage: "known", ManagerIncarnationID: "manager-one", IncarnationID: "sink-one", Sequence: seq, From: at(epoch), Through: at(epoch.Add(10 * time.Hour)), Gaps: []pluginapi.UsageDeliveryGap{}}
}
func apply(t *testing.T, f *fixture, m, id string, seq uint64) {
	t.Helper()
	must(t, f.c.ApplyCheckpoint(ctx, m, tuple(id), response(id, seq)))
}
func snapshot(t *testing.T, f *fixture, m, account string, from, to time.Time) measurement.SourceSnapshot {
	t.Helper()
	s, err := f.c.Snapshot(ctx, m, "claude", account, from, to)
	must(t, err)
	return s
}
func rawUsage(id, account string, requested time.Time) ([]byte, Capture) {
	s := tuple(account)
	e := &pluginapi.UsageSourceEvidence{Scope: s, ManagerIncarnationID: "manager-one", Tracked: true, RequestedAt: requested, UpstreamStartedAt: at(requested.Add(time.Nanosecond)), FinalUsageAt: requested.Add(time.Second)}
	r := map[string]any{"RequestID": id, "Provider": "claude", "AuthID": s.AuthID, "AuthIndex": s.AuthIndex, "Model": "claude-opus-5-5", "RequestedAt": requested, "Latency": int64(time.Second), "Failed": false, "Generate": true, "AccountingVersion": 2, "SourceEvidence": e,
		"TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 250000, "input": map[string]any{"total_tokens": 250000, "uncached_tokens": 250000, "cache_read_tokens": 0, "cache_write_tokens": 0}, "output": map[string]any{"total_tokens": 0, "non_reasoning_tokens": 0, "reasoning_tokens": 0}, "unclassified_tokens": 0}}
	raw, _ := json.Marshal(r)
	return raw, Capture{Credential: measurement.NativeCredential{Provider: "claude", AccountID: account, CredentialRef: "credential-" + account, AuthID: s.AuthID, AuthIndex: s.AuthIndex}, MembershipID: "members-" + account, Verified: true}
}

func TestScopedDurableProofExactAccountAndNanosecondBounds(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	apply(t, f, m.ID, "a", 1)
	s := snapshot(t, f, m.ID, "a", epoch, epoch.Add(10*time.Hour))
	if !s.Known || !s.From.Equal(epoch) || s.From.Nanosecond() != 123 || s.PhysicalCarryInProven {
		t.Fatalf("scoped proof=%+v", s)
	}
	if s = snapshot(t, f, m.ID, "b", epoch, epoch.Add(time.Hour)); s.Known {
		t.Fatal("account A certified B")
	}
	if s = snapshot(t, f, m.ID, "a", epoch, epoch.Add(10*time.Hour+time.Nanosecond)); s.Known {
		t.Fatal("exclusive upper bound expanded")
	}
	if s = snapshot(t, f, m.ID, "a", epoch.Add(-time.Nanosecond), epoch.Add(time.Hour)); s.Known {
		t.Fatal("lower bound expanded")
	}
	var count int
	must(t, f.db.QueryRow(`SELECT count(*) FROM measurement_delivery_runs`).Scan(&count))
	if count != 0 {
		t.Fatal("scoped proof wrote global run")
	}
	// Reopen a store, not a new consumer: durable proof is still available under
	// the same explicit incarnation for inspection, without inferring a new run.
	s2, err := measurement.Open(ctx, f.db, measurement.Options{Clock: func() time.Time { return epoch.Add(24 * time.Hour) }})
	must(t, err)
	s, err = s2.SourceSnapshot(ctx, f.c.ID(), m.ID, "claude", "a", epoch, epoch.Add(time.Hour))
	must(t, err)
	if !s.Known {
		t.Fatal(s)
	}
}

func TestScopedMembershipRequiresEveryTupleAndChangesCutIntervals(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a", "alias-a")
	must(t, f.c.PutMembership(ctx, m))
	apply(t, f, m.ID, "a", 1)
	if snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("partial aliases certified entire account")
	}
	apply(t, f, m.ID, "alias-a", 2)
	if !snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("complete tuple intersection unavailable")
	}
	m2 := membership("members-a-2", "a", "a", "alias-a", "new-a")
	m2.Revision = 2
	m2.From = epoch.Add(2 * time.Hour)
	must(t, f.c.PutMembership(ctx, m2))
	if snapshot(t, f, m.ID, "a", epoch, epoch.Add(3*time.Hour)).Known {
		t.Fatal("old membership crossed change")
	}
	if !snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("membership cutoff erased prior immutable interval")
	}
	if snapshot(t, f, m2.ID, "a", m2.From, m2.From.Add(time.Hour)).Known {
		t.Fatal("new membership borrowed old proofs")
	}
	// A complete tuple already bound to immutable account a cannot be relabelled b.
	foreign := membership("members-b", "b", "a")
	if err := f.c.PutMembership(ctx, foreign); !errors.Is(err, measurement.ErrConflict) {
		t.Fatalf("account alias rebind=%v", err)
	}
}

func TestScopedUnverifiedFallbackAndMissingAliasesStayUnknown(t *testing.T) {
	for _, mode := range []string{"unverified", "incomplete", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			m := membership("members-a", "a", "a")
			switch mode {
			case "unverified":
				m.Verified = false
				m.MappingEvidenceRef = ""
			case "incomplete":
				m.Complete = false
			case "empty":
				m.Members = nil
			}
			must(t, f.c.PutMembership(ctx, m))
			if mode != "empty" {
				apply(t, f, m.ID, "a", 1)
			}
			if snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
				t.Fatal("unproven account mapping certified")
			}
		})
	}
	f := newFixture(t)
	m := membership("partial", "a", "a")
	m.Members[0].Scope.AuthID = ""
	if err := f.c.PutMembership(ctx, m); !errors.Is(err, measurement.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestScopedLateOldGapAndTruncatedInvalidFromSurviveRestart(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	must(t, f.c.PutMembership(ctx, membership("members-b", "b", "b")))
	apply(t, f, m.ID, "a", 10)
	apply(t, f, "members-b", "b", 10)
	r := response("a", 9)
	r.Coverage = "unknown"
	r.Through = nil
	r.GapCount = 20
	r.GapsTruncated = true
	r.InvalidFrom = at(epoch.Add(-time.Hour))
	r.Gaps = []pluginapi.UsageDeliveryGap{{Sequence: 8, Reason: "untracked_publication", From: at(epoch.Add(time.Hour)), ObservedAt: epoch.Add(2 * time.Hour)}}
	if err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r); !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatalf("stale response=%v", err)
	}
	s := snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour))
	if s.Known || s.InvalidFrom == nil || !s.InvalidFrom.Equal(*r.InvalidFrom) {
		t.Fatal(s)
	}
	if !snapshot(t, f, "members-b", "b", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("A gap poisoned B")
	}
	apply(t, f, m.ID, "a", 11)
	if snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("later success erased sticky gap")
	}
	must(t, f.db.Close())
	f2 := openFixture(t, f.path, epoch.Add(3*time.Hour))
	apply(t, f2, m.ID, "a", 12)
	s = snapshot(t, f2, m.ID, "a", epoch.Add(4*time.Hour), epoch.Add(5*time.Hour))
	if s.Known || s.InvalidFrom == nil || !s.InvalidFrom.Equal(*r.InvalidFrom) {
		t.Fatalf("restart lost invalidation=%+v", s)
	}
}

func TestScopedUnknownStartRevokesEntireIncarnationAndCannotNarrow(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	apply(t, f, m.ID, "a", 1)
	r := response("a", 2)
	r.Coverage = "unknown"
	r.Through = nil
	r.GapCount = 1
	r.Gaps = []pluginapi.UsageDeliveryGap{{Sequence: 2, Reason: "missing_identity", ObservedAt: epoch.Add(time.Hour)}}
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	r.Sequence = 3
	r.Gaps[0].Sequence = 3
	r.InvalidFrom = at(epoch.Add(time.Hour))
	r.Gaps[0].From = r.InvalidFrom
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	s := snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour))
	if s.Known || !s.HasGap || s.InvalidFrom != nil {
		t.Fatal(s)
	}
}

func TestScopedSinkManagerAndConsumerIncarnationsCannotBorrowProof(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	r := response("a", 1)
	r.Through = at(epoch.Add(time.Hour))
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	r2 := response("a", 2)
	r2.IncarnationID = "sink-two"
	r2.From = at(epoch.Add(2 * time.Hour))
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r2))
	if snapshot(t, f, m.ID, "a", epoch, epoch.Add(3*time.Hour)).Known {
		t.Fatal("sink reload borrowed old window")
	}
	if !snapshot(t, f, m.ID, "a", epoch.Add(3*time.Hour), epoch.Add(4*time.Hour)).Known {
		t.Fatal("new sink proof missing")
	}
	r.Sequence = 3
	if err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r); !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatal(err)
	}
	r3 := response("a", 1)
	r3.ManagerIncarnationID = "manager-two"
	r3.IncarnationID = "sink-three"
	r3.From = at(epoch.Add(11 * time.Hour))
	r3.Through = at(epoch.Add(12 * time.Hour))
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r3))
	if snapshot(t, f, m.ID, "a", epoch.Add(3*time.Hour), epoch.Add(4*time.Hour)).Known {
		t.Fatal("manager restart borrowed history")
	}
	newConsumer, err := New(ctx, f.store, Options{SourceID: "host-installation", Clock: func() time.Time { return epoch.Add(11 * time.Hour) }})
	must(t, err)
	if snapshot(t, f, m.ID, "a", epoch.Add(11*time.Hour), epoch.Add(12*time.Hour)).Known {
		t.Fatal("retired consumer used new proof")
	}
	f.c = newConsumer
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r3))
	if !snapshot(t, f, m.ID, "a", epoch.Add(11*time.Hour), epoch.Add(12*time.Hour)).Known {
		t.Fatal("new consumer cannot store independent window")
	}
	if err = f.store.StartSourceConsumer(ctx, newConsumer.ID(), "host-installation", epoch.Add(11*time.Hour)); !errors.Is(err, measurement.ErrConflict) {
		t.Fatal("consumer ID reused", err)
	}
}

func TestScopedDuplicateConflictingAndOutOfOrderCheckpoints(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	apply(t, f, m.ID, "a", 2)
	apply(t, f, m.ID, "a", 2)
	if err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), response("a", 1)); !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatal(err)
	}
	if !snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("harmless stale response damaged proof")
	}
	r := response("a", 2)
	r.Through = at(epoch.Add(11 * time.Hour))
	if err := f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r); !errors.Is(err, measurement.ErrConflict) {
		t.Fatal(err)
	}
	if snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("same-sequence conflict retained known proof")
	}
}

func TestScopedGapDBFailureFusesBeforeNextReadOrAdvance(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	apply(t, f, m.ID, "a", 1)
	_, err := f.db.Exec(`CREATE TRIGGER fail_gap BEFORE INSERT ON measurement_source_gaps BEGIN SELECT RAISE(FAIL,'dummy gap write failure'); END`)
	must(t, err)
	r := response("a", 2)
	r.Coverage = "unknown"
	r.Through = nil
	r.GapCount = 1
	r.GapsTruncated = true
	if err = f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r); err == nil {
		t.Fatal("gap failure acknowledged")
	}
	_, err = f.db.Exec(`DROP TRIGGER fail_gap`)
	must(t, err)
	if snapshot(t, f, m.ID, "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("proof readable before failed invalidation persisted")
	}
	if err = f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), response("a", 3)); !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatal(err)
	}
	var poisoned bool
	must(t, f.db.QueryRow(`SELECT poisoned FROM measurement_source_consumers WHERE id=?`, f.c.ID()).Scan(&poisoned))
	if !poisoned {
		t.Fatal("failure was not durable")
	}
}

func TestScopedObserveUsagePersistsUnmanagedClocksAndDuplicates(t *testing.T) {
	f := newFixture(t)
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	raw, capture := rawUsage("attempt-one", "a", epoch.Add(time.Minute))
	result, err := f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	if !result.Inserted {
		t.Fatal(result)
	}
	result, err = f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	if !result.Duplicate {
		t.Fatal(result)
	}
	r, err := measurement.FromNative(raw, capture.Credential, "host-installation")
	must(t, err)
	stored, err := f.store.Record(ctx, r.EventID, 0)
	must(t, err)
	if stored.StartTimeBasis != "upstream" || !stored.StartedAt.Equal(r.RequestedAt.Add(time.Nanosecond)) || stored.EndTimeBasis != "source_final_usage_observation" || !stored.Native.BillingMappingVerified || stored.Native.SourceMembershipID != "members-a" {
		t.Fatalf("provenance=%+v", stored)
	}
	b, err := json.Marshal(stored)
	must(t, err)
	if strings.Contains(string(b), tuple("a").AuthID) {
		t.Fatal("raw AuthID retained")
	}
	apply(t, f, "members-a", "a", 1)
	if !snapshot(t, f, "members-a", "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("persisted tracked usage prevented source proof")
	}
}

func TestScopedLateUsageRecordGapInvalidatesAllSinkIncarnations(t *testing.T) {
	f := newFixture(t)
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	apply(t, f, "members-a", "a", 10)
	raw, capture := rawUsage("late", "a", epoch.Add(time.Minute))
	var envelope map[string]any
	must(t, json.Unmarshal(raw, &envelope))
	e := envelope["SourceEvidence"].(map[string]any)
	e["gap"] = map[string]any{"sequence": 5, "reason": "SECRET-NOT-RETAINED", "from": epoch, "observed_at": epoch.Add(time.Minute)}
	raw, err := json.Marshal(envelope)
	must(t, err)
	_, err = f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	r := response("a", 11)
	r.IncarnationID = "sink-two"
	must(t, f.c.ApplyCheckpoint(ctx, "members-a", tuple("a"), r))
	if snapshot(t, f, "members-a", "a", epoch.Add(time.Hour), epoch.Add(2*time.Hour)).Known {
		t.Fatal("new sink erased manager-wide usage gap")
	}
	var body string
	must(t, f.db.QueryRow(`SELECT body FROM measurement_records`).Scan(&body))
	if strings.Contains(body, "SECRET-NOT-RETAINED") {
		t.Fatal("arbitrary gap reason retained")
	}
}

func TestScopedMissingCorrelationErrorsAndLegacyRecordsRemainValuable(t *testing.T) {
	f := newFixture(t)
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	apply(t, f, "members-a", "a", 1)
	raw, capture := rawUsage("legacy", "a", epoch.Add(time.Minute))
	var envelope map[string]any
	must(t, json.Unmarshal(raw, &envelope))
	delete(envelope, "SourceEvidence")
	raw, err := json.Marshal(envelope)
	must(t, err)
	result, err := f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	if !result.Inserted {
		t.Fatal(result)
	}
	if snapshot(t, f, "members-a", "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("legacy path obtained source coverage")
	}
	delete(envelope, "RequestID")
	raw, err = json.Marshal(envelope)
	must(t, err)
	if _, err = f.c.ObserveUsage(ctx, raw, capture); !errors.Is(err, measurement.ErrUnrecordable) {
		t.Fatal("uncorrelated record silently dropped", err)
	}
	if _, err = f.c.ObserveUsage(ctx, raw, Capture{}); err == nil {
		t.Fatal("missing credential correlation acknowledged")
	}
	var count int
	must(t, f.db.QueryRow(`SELECT count(*) FROM measurement_records`).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
}

func TestScopedPersistenceFailureMustReachParentHook(t *testing.T) {
	f := newFixture(t)
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	_, err := f.db.Exec(`CREATE TRIGGER fail_record BEFORE INSERT ON measurement_records BEGIN SELECT RAISE(FAIL,'dummy record failure'); END`)
	must(t, err)
	raw, capture := rawUsage("failed-write", "a", epoch.Add(time.Minute))
	if _, err = f.c.ObserveUsage(ctx, raw, capture); err == nil {
		t.Fatal("unrecorded tracked callback returned nil")
	}
	_, err = f.db.Exec(`DROP TRIGGER fail_record`)
	must(t, err)
	if err = f.c.ApplyCheckpoint(ctx, "members-a", tuple("a"), response("a", 1)); !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatal(err)
	}
	if snapshot(t, f, "members-a", "a", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("failed sink obtained proof")
	}
}

func TestScopedNativeMeasurementNeverBorrowsGlobalRuns(t *testing.T) {
	f := newFixture(t)
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	raw, capture := rawUsage("priced", "a", epoch.Add(time.Minute))
	_, err := f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	apply(t, f, "members-a", "a", 1)
	must(t, f.store.RegisterPriceBook(ctx, measurement.DefaultPriceBook()))
	must(t, f.store.StartDelivery(ctx, "synthetic-global", epoch.Add(-time.Hour)))
	must(t, f.store.Checkpoint(ctx, "synthetic-global", epoch.Add(12*time.Hour), false))
	for i, when := range []time.Time{epoch, epoch.Add(time.Hour)} {
		must(t, f.store.PutObservation(ctx, measurement.Observation{ID: []string{"before", "after"}[i], Provider: "claude", AccountID: "a", Scope: "all", CycleID: "cycle", Duration: 5 * time.Hour, At: when, UsedPercent: float64(i * 20), Usable: true, Source: "dummy-header"}))
	}
	must(t, f.store.CloseCycle(ctx, measurement.Closure{CycleRef: measurement.CycleRef{Provider: "claude", AccountID: "a", Scope: "all", CycleID: "cycle", Duration: 5 * time.Hour}, At: epoch.Add(time.Hour), Source: "dummy-reset"}))
	q := measurement.Query{Provider: "claude", AccountID: "a", Scope: "all", BookID: measurement.DefaultPriceBook().ID, Since: epoch, AsOf: epoch.Add(time.Hour)}
	for _, explicit := range []bool{false, true} {
		q2 := q
		if explicit {
			q2, err = f.c.BeforeMeasure(ctx, q, map[string]string{"a": "members-a"})
			must(t, err)
		}
		r, err := f.store.Measure(ctx, q2)
		must(t, err)
		if r.Coverage.Observed || r.FiveHour.Known || r.WarmBurn.Bounded || r.Coverage.Delivery != "cpa-scoped-request-start" {
			t.Fatalf("native measurement borrowed global proof: %+v", r)
		}
		if explicit && !strings.Contains(strings.Join(r.Coverage.Reasons, ","), "physical_carry_in_unproven") {
			t.Fatal(r.Coverage)
		}
	}
}

func TestScopedConcurrentOutOfOrderCheckpointsAndLimitFailure(t *testing.T) {
	f := newFixture(t)
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 1; i <= 16; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			errs <- f.c.ApplyCheckpoint(ctx, "members-a", tuple("a"), response("a", seq))
		}(uint64(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, measurement.ErrStaleSource) {
			t.Fatal(err)
		}
	}
	s := snapshot(t, f, "members-a", "a", epoch, epoch.Add(time.Hour))
	if !s.Known || s.Runs[0].Sequence != 16 {
		t.Fatal(s)
	}
	must(t, f.c.PutMembership(ctx, membership("members-b", "b", "b")))
	_, err := f.db.Exec(`UPDATE measurement_source_limits SET runs=8192 WHERE id=1`)
	must(t, err)
	if err = f.c.ApplyCheckpoint(ctx, "members-b", tuple("b"), response("b", 1)); !errors.Is(err, measurement.ErrLimit) {
		t.Fatal(err)
	}
	if snapshot(t, f, "members-b", "b", epoch, epoch.Add(time.Hour)).Known {
		t.Fatal("tracking overflow certified")
	}
}

func TestScopedMalformedCheckpointAndRPCFailureRevokeKnown(t *testing.T) {
	for _, mode := range []string{"foreign_scope", "version", "missing_from", "missing_through", "reverse", "future", "manager", "sink", "sequence", "inconsistent_gaps", "rpc"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
			apply(t, f, "members-a", "a", 1)
			r := response("a", 2)
			switch mode {
			case "foreign_scope":
				r.Scope.AuthIndex = "another-index"
			case "version":
				r.Scope.Version = 2
			case "missing_from":
				r.From = nil
			case "missing_through":
				r.Through = nil
			case "reverse":
				r.Through = at(epoch.Add(-time.Hour))
			case "future":
				r.Through = at(epoch.Add(25 * time.Hour))
			case "manager":
				r.ManagerIncarnationID = ""
			case "sink":
				r.IncarnationID = ""
			case "sequence":
				r.Sequence = 0
			case "inconsistent_gaps":
				r.GapCount = 1
			case "rpc":
				must(t, f.c.CheckpointFailed(ctx, "members-a"))
			}
			if mode != "rpc" {
				if err := f.c.ApplyCheckpoint(ctx, "members-a", tuple("a"), r); err == nil {
					t.Fatal("invalid proof accepted")
				}
			}
			if snapshot(t, f, "members-a", "a", epoch, epoch.Add(time.Hour)).Known {
				t.Fatal("invalid response retained proof")
			}
		})
	}
}

func TestScopedMembershipCannotResetIncarnationSequenceTracking(t *testing.T) {
	f := newFixture(t)
	m := membership("members-a", "a", "a")
	must(t, f.c.PutMembership(ctx, m))
	r := response("a", 10)
	r.Through = at(epoch.Add(time.Hour))
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r))
	r2 := response("a", 11)
	r2.IncarnationID = "sink-two"
	r2.From = at(epoch.Add(2 * time.Hour))
	must(t, f.c.ApplyCheckpoint(ctx, m.ID, tuple("a"), r2))
	m2 := membership("members-a-2", "a", "a")
	m2.Revision = 2
	m2.From = epoch.Add(2 * time.Hour)
	must(t, f.c.PutMembership(ctx, m2))
	r.Sequence = 12
	if err := f.c.ApplyCheckpoint(ctx, m2.ID, tuple("a"), r); !errors.Is(err, measurement.ErrStaleSource) {
		t.Fatal("fresh membership reactivated retired sink", err)
	}
	must(t, f.c.ApplyCheckpoint(ctx, m2.ID, tuple("a"), r2))
	if !snapshot(t, f, m2.ID, "a", epoch.Add(3*time.Hour), epoch.Add(4*time.Hour)).Known {
		t.Fatal("current sink cannot issue proof for explicit new membership")
	}
}

func TestScopedRequestedCoverageDoesNotCertifyPhysicalCarryIn(t *testing.T) {
	f := openFixture(t, filepath.Join(t.TempDir(), "carry.sqlite"), epoch.Add(2*time.Hour))
	must(t, f.c.PutMembership(ctx, membership("members-a", "a", "a")))
	raw, capture := rawUsage("carry-in", "a", epoch.Add(time.Hour))
	var envelope map[string]any
	must(t, json.Unmarshal(raw, &envelope))
	envelope["Latency"] = int64(3 * time.Hour)
	e := envelope["SourceEvidence"].(map[string]any)
	e["upstream_started_at"] = epoch.Add(3 * time.Hour)
	e["final_usage_at"] = epoch.Add(4 * time.Hour)
	raw, err := json.Marshal(envelope)
	must(t, err)
	_, err = f.c.ObserveUsage(ctx, raw, capture)
	must(t, err)
	apply(t, f, "members-a", "a", 1)
	s := snapshot(t, f, "members-a", "a", epoch.Add(2*time.Hour), epoch.Add(5*time.Hour))
	if !s.Known || s.PhysicalCarryInProven {
		t.Fatal("request-domain proof was silently relabelled physical", s)
	}
	must(t, f.store.RegisterPriceBook(ctx, measurement.DefaultPriceBook()))
	q, err := f.c.BeforeMeasure(ctx, measurement.Query{Provider: "claude", AccountID: "a", Scope: "all", BookID: measurement.DefaultPriceBook().ID, Since: epoch.Add(2 * time.Hour), AsOf: epoch.Add(5 * time.Hour)}, map[string]string{"a": "members-a"})
	must(t, err)
	r, err := f.store.Measure(ctx, q)
	must(t, err)
	if r.Coverage.Observed || !strings.Contains(strings.Join(r.Coverage.Reasons, ","), "physical_carry_in_unproven") {
		t.Fatal(r.Coverage)
	}
}
