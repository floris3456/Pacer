package pacer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/dryrun"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var dryRunDay = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

const dryRunReportPath = "/v0/management/plugins/pacer/dry-run-report"

type dryRunRig struct {
	p      *Plugin
	path   string
	clock  atomic.Int64
	mu     sync.Mutex
	calls  int
	closed bool
}

func (r *dryRunRig) now() time.Time          { return time.Unix(0, r.clock.Load()).UTC() }
func (r *dryRunRig) advance(d time.Duration) { r.clock.Add(int64(d)) }
func (r *dryRunRig) set(t time.Time)         { r.clock.Store(t.UnixNano()) }

func (r *dryRunRig) host(method string, input any) (json.RawMessage, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return testHost(method, input)
}

func (r *dryRunRig) hostCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newDryRunRig(t *testing.T, mode string) *dryRunRig {
	t.Helper()
	r := &dryRunRig{path: filepath.Join(t.TempDir(), "state.sqlite")}
	r.set(dryRunDay)
	r.start(t, mode)
	t.Cleanup(func() {
		if !r.closed {
			r.p.Close()
		}
	})
	return r
}

func (r *dryRunRig) start(t *testing.T, mode string) {
	t.Helper()
	p := New(r.host)
	p.now = r.now
	p.workerOnce.Do(func() {}) // tests drive heartbeats explicitly
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\npriority: 1\nstate_path: " + r.path + "\nrouting:\n  mode: " + mode + "\n")})
	r.p, r.closed = p, false
}

func (r *dryRunRig) restart(t *testing.T, mode string) {
	t.Helper()
	r.p.Close()
	r.start(t, mode)
}

// heartbeats advances the clock in steps, writing one worker heartbeat per step.
func (r *dryRunRig) heartbeats(until time.Time, step time.Duration) {
	for r.now().Before(until) {
		r.advance(step)
		r.p.dryRunHeartbeat()
	}
}

func (r *dryRunRig) decide(t *testing.T, request, chat string, actual credential, proposed routing.Decision) {
	t.Helper()
	ticket := attempt{Key: chat, Model: "claude-opus-5-5", PreviousAccount: actual.Account, Decision: proposed}
	r.p.recordDecision(request, ticket, actual, "admitted")
}

func (r *dryRunRig) report(t *testing.T, query url.Values, want int) map[string]json.RawMessage {
	t.Helper()
	result := invoke(t, r.p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: dryRunReportPath, Query: query})
	response, ok := result.(pluginapi.ManagementResponse)
	if !ok || response.StatusCode != want {
		t.Fatalf("status %d (want %d): %s", response.StatusCode, want, response.Body)
	}
	if response.Headers.Get("Cache-Control") != "no-store" || response.Headers.Get("Content-Type") != "application/json" {
		t.Fatal("headers", response.Headers)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(response.Body, &out); err != nil {
		t.Fatal(err, string(response.Body))
	}
	return out
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err, string(raw))
	}
	return v
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func stateFiles(r *dryRunRig) []string {
	evidence := dryRunEvidencePath(r.path)
	return []string{r.path, r.path + "-wal", evidence, evidence + "-wal"}
}

func TestDryRunDecisionCarriesRequestAndActualAccount(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	b := r.p.creds["b"]
	r.decide(t, "lifecycle-1", "opaque-chat", b, routing.Decision{Handled: true, Auth: "a", Account: r.p.creds["a"].Account, Reason: "cold_move"})
	rows, err := r.p.decisionView()
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if rows[0].RequestID != "lifecycle-1" || rows[0].ActualAccount != b.Account || rows[0].ActualAuth != "b" {
		t.Fatalf("decision: %+v", rows[0])
	}
}

func TestDryRunReportRouteIsReadOnly(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	a, b := r.p.creds["a"], r.p.creds["b"]
	r.heartbeats(dryRunDay.Add(time.Hour), 30*time.Second)
	r.decide(t, "req-1", "chat-1", a, routing.Decision{Handled: true, Auth: "b", Account: b.Account, Reason: "cold_move"})
	r.heartbeats(dryRunDay.Add(2*time.Hour), 30*time.Second)
	r.set(dryRunDay.Add(25 * time.Hour))
	before := map[string]string{}
	for _, path := range stateFiles(r) {
		before[path] = fileDigest(t, path)
	}
	calls := r.hostCalls()
	for i := 0; i < 3; i++ {
		out := r.report(t, url.Values{"day": {"2026-10-07"}}, 200)
		meta := decode[dryrun.Meta](t, out["meta"])
		moves := decode[dryrun.Moves](t, out["would_be_moves"])
		if meta.Day != "2026-10-07" || meta.Decisions != 1 || meta.Complete || moves.Total != 1 || moves.Items[0].To != b.Account || moves.Items[0].From != a.Account {
			t.Fatalf("report: %+v %+v", meta, moves)
		}
		if moves.Items[0].ChatRef != dryrun.ChatRef(r.p.dryRun.Secret(), "chat-1") {
			t.Fatal("chat_ref is not keyed by the installation secret")
		}
	}
	for _, path := range stateFiles(r) {
		if after := fileDigest(t, path); after != before[path] {
			t.Fatalf("report changed %s", filepath.Base(path))
		}
	}
	if got := r.hostCalls(); got != calls {
		t.Fatalf("report made %d host calls", got-calls)
	}
}

func TestDryRunReportDayParameter(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	r.set(dryRunDay.Add(30 * time.Hour))
	if meta := decode[dryrun.Meta](t, r.report(t, nil, 200)["meta"]); meta.Day != "2026-10-07" {
		t.Fatal("default day must be yesterday UTC", meta.Day)
	}
	if meta := decode[dryrun.Meta](t, r.report(t, url.Values{"day": {"2026-10-08"}}, 200)["meta"]); meta.Day != "2026-10-08" || meta.Complete {
		t.Fatalf("today is in progress: %+v", meta)
	}
	for _, bad := range []string{"2026-10-09", "2026-10-7", "2026-13-01", "yesterday", "2026-10-07T00:00:00Z"} {
		r.report(t, url.Values{"day": {bad}}, 400)
	}
	r.report(t, url.Values{"day": {"2026-10-07", "2026-10-06"}}, 400)
}

func TestDryRunReportRouteIsRegistered(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	raw, err := json.Marshal(invoke(t, r.p, pluginabi.MethodManagementRegister, nil))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Routes []map[string]string }
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, route := range manifest.Routes {
		if route["Path"] == "/plugins/pacer/dry-run-report" {
			found++
			if route["Method"] != "GET" {
				t.Fatal("dry-run report must be GET only", route)
			}
		}
	}
	if found != 1 {
		t.Fatal("dry-run report route registered", found, manifest.Routes)
	}
	r.report(t, nil, 200)
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		result := invoke(t, r.p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: method, Path: dryRunReportPath})
		if response := result.(pluginapi.ManagementResponse); response.StatusCode != 404 {
			t.Fatal(method, response.StatusCode)
		}
	}
}

// Over the read limit, the oldest decisions are dropped at a whole timestamp
// and the lookback start moves forward; the report day itself must fit.
func TestDryRunDecisionReadLimitShortensLookback(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	for _, at := range []time.Duration{-3 * time.Hour, -2 * time.Hour, -2 * time.Hour, -time.Hour, time.Hour} {
		body, _ := json.Marshal(decisionEntry{At: dryRunDay.Add(at), Chat: "chat", Provider: "claude", Outcome: "admitted"})
		if _, err := r.p.db.Exec(`INSERT INTO pacer_decisions(at,body) VALUES(?,?)`, dryRunDay.Add(at).UnixNano(), body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, since, end := context.Background(), dryRunDay.Add(-dryrun.MaxLookback), dryRunDay.Add(24*time.Hour)
	all, start, err := readDryRunDecisions(ctx, r.p.db, since, dryRunDay, end, 10)
	if err != nil || len(all) != 5 || !start.Equal(since) || !all[0].At.Equal(dryRunDay.Add(-3*time.Hour)) || all[1].Seq >= all[2].Seq {
		t.Fatal("ascending (at, seq) without a limit", all, start, err)
	}
	got, start, err := readDryRunDecisions(ctx, r.p.db, since, dryRunDay, end, 3)
	if err != nil || len(got) != 2 || !start.Equal(dryRunDay.Add(-2*time.Hour+time.Nanosecond)) || !got[0].At.Equal(dryRunDay.Add(-time.Hour)) {
		t.Fatal("a timestamp is never split by the limit", got, start, err)
	}
	if _, _, err = readDryRunDecisions(ctx, r.p.db, dryRunDay, dryRunDay, end, 1); err != nil {
		t.Fatal("one in-day decision fits a limit of one", err)
	}
	if _, _, err = readDryRunDecisions(ctx, r.p.db, since, dryRunDay.Add(-90*time.Minute), end, 1); !errors.Is(err, errDryRunDayTooLarge) {
		t.Fatal("a report day over the limit must fail", err)
	}
}

func TestDryRunReportRefusesWithoutEvidenceLedger(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	r.p.mu.Lock()
	r.p.dryRun = nil
	r.p.mu.Unlock()
	r.report(t, url.Values{"day": {"2026-10-07"}}, 503)
}

func TestDryRunModeMarksProveCoverage(t *testing.T) {
	r := newDryRunRig(t, "off")
	r.heartbeats(dryRunDay.Add(time.Hour), 5*time.Minute) // off: no proof
	if err := r.p.setMode("dry-run"); err != nil {
		t.Fatal(err)
	}
	r.heartbeats(dryRunDay.Add(3*time.Hour), 5*time.Minute)
	// A failed decision write is counted, carried by the next heartbeat, and
	// marks that segment. query_only fails the write without DDL, which the
	// measurement feed witness would refuse; the state pool has one connection.
	if _, err := r.p.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	r.decide(t, "lost", "chat", r.p.creds["a"], routing.Decision{Handled: true, Auth: "a", Account: r.p.creds["a"].Account, Reason: "new_chat"})
	if _, err := r.p.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if r.p.decisionErrors != 1 {
		t.Fatal("decision write failure was not counted", r.p.decisionErrors)
	}
	r.heartbeats(dryRunDay.Add(4*time.Hour), 5*time.Minute)
	if err := r.p.setMode("off"); err != nil {
		t.Fatal(err)
	}
	r.heartbeats(dryRunDay.Add(5*time.Hour), 5*time.Minute)
	// A restart in dry-run starts a new run; the gap before it is not covered.
	r.advance(time.Minute)
	r.restart(t, "dry-run")
	r.heartbeats(dryRunDay.Add(6*time.Hour+time.Minute), 5*time.Minute)
	r.set(dryRunDay.Add(25 * time.Hour))
	meta := decode[dryrun.Meta](t, r.report(t, url.Values{"day": {"2026-10-07"}}, 200)["meta"])
	if len(meta.Coverage) != 1 || meta.Coverage[0].Provider != "claude" {
		t.Fatalf("coverage: %+v", meta.Coverage)
	}
	c := meta.Coverage[0]
	if c.CoveredSeconds != 4*3600 || c.Complete || len(c.Intervals) != 2 {
		t.Fatalf("covered: %+v", c)
	}
	if !c.Intervals[0].From.Equal(dryRunDay.Add(time.Hour)) || !c.Intervals[0].To.Equal(dryRunDay.Add(4*time.Hour)) || !c.Intervals[1].From.Equal(dryRunDay.Add(5*time.Hour+time.Minute)) {
		t.Fatalf("intervals: %+v", c.Intervals)
	}
	for _, want := range []string{"dry_run_coverage_partial", "decision_write_errors"} {
		found := false
		for _, reason := range meta.Reasons {
			found = found || reason == want
		}
		if !found {
			t.Fatalf("missing %s: %+v", want, meta.Reasons)
		}
	}
}

// A one-hour cycle of account A observed at 30% used, a $2 request Pacer would
// have placed on A instead of B, and a measured $0.50 per percent: the
// projection is 70 - 2*(2-0) = 66% unused at the reset.
func TestDryRunReportProjectsMeasuredReset(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	a, b := r.p.creds["a"], r.p.creds["b"]
	reset := dryRunDay.Add(2 * time.Hour)
	ref := measurement.CycleRef{Provider: "claude", AccountID: a.Account, Scope: "all", CycleID: reset.Format(time.RFC3339Nano), Duration: time.Hour}
	r.heartbeats(dryRunDay.Add(70*time.Minute), 5*time.Minute)
	r.decide(t, "req-in", "chat-in", b, routing.Decision{Handled: true, Auth: "a", Account: a.Account, Reason: "new_chat"})
	start := r.now()
	ctx := context.Background()
	record := measurement.Record{EventID: "event-in", SourceID: "test-source", Revision: 1, RequestID: "req-in", AttemptID: "attempt-in",
		Attribution: measurement.Attribution{Provider: "claude", AccountID: b.Account, Model: "claude-opus-5-5"}, Kind: "primary",
		StartedAt: start, EndedAt: start.Add(5 * time.Second), Final: true, Success: true,
		Usage: measurement.Usage{AccountingVersion: "normalized-v1", Quality: "complete", NonCacheInput: 2 * 250000}}
	if _, err := r.p.measurement.PutRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	r.heartbeats(dryRunDay.Add(110*time.Minute), 5*time.Minute)
	o := measurement.Observation{ID: "obs-a", Provider: "claude", AccountID: a.Account, Scope: "all", CycleID: ref.CycleID, Duration: time.Hour, At: r.now(), UsedPercent: 30, Source: "header", Usable: true}
	if err := r.p.measurement.PutObservation(ctx, o); err != nil {
		t.Fatal(err)
	}
	r.heartbeats(reset.Add(30*time.Minute), 5*time.Minute)
	if err := r.p.measurement.CloseCycle(ctx, measurement.Closure{CycleRef: ref, At: reset.Add(10 * time.Second), Source: "observed-new-cycle"}); err != nil {
		t.Fatal(err)
	}
	r.p.mu.Lock()
	r.p.measurements[a.Account] = measurement.Report{AsOf: reset.Add(time.Minute), Windows: []measurement.WindowEstimate{{CycleRef: ref, Eligible: true, USDPerPercent: 0.5}}}
	r.p.mu.Unlock()
	r.set(dryRunDay.Add(25 * time.Hour))
	resets := decode[[]dryrun.Reset](t, r.report(t, url.Values{"day": {"2026-10-07"}}, 200)["resets"])
	if len(resets) != 1 {
		t.Fatalf("resets: %+v", resets)
	}
	got := resets[0]
	if got.Account != a.Account || !got.ResetAt.Equal(reset) || got.ResetBasis != "scheduled" || got.Baseline.Status != "fresh" || got.Baseline.UnusedPercent == nil || *got.Baseline.UnusedPercent != 70 {
		t.Fatalf("reset: %+v", got)
	}
	p := got.Projection
	if !p.Known || p.UnusedPercent == nil || math.Abs(*p.UnusedPercent-66) > 1e-9 || p.RequestsIn != 1 || p.ValueInUSD != 2 {
		t.Fatalf("projection: %+v", p)
	}
	// Without the measured ratio the same reset stays explicitly unknown.
	r.p.mu.Lock()
	delete(r.p.measurements, a.Account)
	r.p.mu.Unlock()
	resets = decode[[]dryrun.Reset](t, r.report(t, url.Values{"day": {"2026-10-07"}}, 200)["resets"])
	if p = resets[0].Projection; p.Known || len(p.Reasons) == 0 {
		t.Fatalf("projection without ratio: %+v", p)
	}
}

func TestDryRunEvidenceNeverChangesStateSchema(t *testing.T) {
	r := newDryRunRig(t, "dry-run")
	r.p.Close()
	r.closed = true
	version := func() (v int64, objects int64) {
		db, err := sql.Open("sqlite3", "file:"+r.path+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err = db.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'dry_run%'`).Scan(&objects); err != nil {
			t.Fatal(err)
		}
		return
	}
	before, objects := version()
	if objects != 0 {
		t.Fatalf("state database contains %d dry-run objects", objects)
	}
	info, err := os.Stat(dryRunEvidencePath(r.path))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("dedicated owner-only dry-run evidence database missing", err)
	}
	r.start(t, "dry-run")
	secret := append([]byte(nil), r.p.dryRun.Secret()...)
	r.restart(t, "dry-run")
	if string(secret) != string(r.p.dryRun.Secret()) {
		t.Fatal("installation secret changed across restart")
	}
	r.p.Close()
	r.closed = true
	if after, objects := version(); after != before || objects != 0 {
		t.Fatalf("state schema generation changed: %d -> %d (%d dry-run objects)", before, after, objects)
	}
}
