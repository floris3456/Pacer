package pacer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type forwardFixture struct {
	h                         *coverageHostFixture
	state, path, book, source string
	c                         credential
	ticks                     chan time.Time
}

func preparedRuntime(t *testing.T) forwardFixture {
	t.Helper()
	dir := t.TempDir()
	f := forwardFixture{h: newCoverageHost(1), state: filepath.Join(dir, "state.sqlite"), path: filepath.Join(dir, "forward.sqlite"), ticks: make(chan time.Time, 8)}
	p := openCoveragePlugin(t, f.h, f.state)
	f.c = p.creds["auth-000"]
	f.source = p.measurementSource
	if p.ForwardStatus().State != "disabled" || p.forward != nil {
		t.Fatal("unconfigured forward opened")
	}
	p.Close()
	s, e := historyreports.OpenForward(context.Background(), f.path)
	if e != nil {
		t.Fatal(e)
	}
	book := historyreports.ForwardBook{ID: "synthetic-authorized-v1", Book: historyreports.ForwardComparisonBook{ID: "synthetic-authorized-v1", Policy: "profile-api-value-v3", Prices: map[string]historyreports.ForwardModelPrice{"claude-opus-5-5": {Prompt: 4, Completion: 20, CacheRead: .2}}}, Qualifications: []historyreports.ForwardPriceQualification{{SourceID: f.source, Provider: f.c.Provider, Model: "claude-opus-5-5", AccountID: f.c.Account, CredentialRef: tracker.Digest(f.c.Provider + "\x00" + f.c.ID + "\x00" + f.c.Index)}}}
	f.book, e = s.RegisterForwardBook(context.Background(), book)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	return f
}
func (f forwardFixture) config() []byte {
	return []byte("enabled: true\nstate_path: " + f.state + "\nforward_report_path: " + f.path + "\nforward_report_book: " + f.book + "\nrouting:\n  mode: off\n")
}
func (f forwardFixture) open(t *testing.T, edit func(*forwardOptions)) *Plugin {
	t.Helper()
	p := New(f.h.call)
	p.now = f.h.now
	p.workerOnce.Do(func() {})
	opts := defaultForwardOptions()
	opts.ticks = f.ticks
	opts.yield = 0
	opts.retry = 10 * time.Millisecond
	if edit != nil {
		edit(&opts)
	}
	p.forwardOptions = &opts
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": f.config()})
	t.Cleanup(p.Close)
	return p
}
func forwardWait(t *testing.T, p *Plugin, predicate func(ForwardRuntimeStatus) bool) ForwardRuntimeStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		v, ch := p.forward.snapshot()
		if predicate(v) {
			return v
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("forward wait expired: %+v", v)
		}
	}
}
func forwardMixed(c credential, logical, physical string, at time.Time) map[string]any {
	u := hookUsage(c, logical, physical, at)
	u["Detail"] = map[string]any{"CacheCreation": map[string]any{"total_tokens": 1000000, "ephemeral_5m_input_tokens": 250000, "ephemeral_1h_input_tokens": 750000, "unknown_tokens": 0, "quality": "complete"}}
	u["TokenBreakdown"] = map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 1000000, "input": map[string]any{"total_tokens": 1000000, "uncached_tokens": 0, "cache_read_tokens": 0, "cache_write_tokens": 1000000}, "output": map[string]any{"total_tokens": 0, "non_reasoning_tokens": 0, "reasoning_tokens": 0}, "unclassified_tokens": 0}
	return u
}
func forwardReportAt(t *testing.T, p *Plugin, generation string) historyreports.ForwardReport {
	t.Helper()
	var report historyreports.ForwardReport
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.WithForwardReports(ctx, func(ctx context.Context, s *historyreports.ForwardStore) error {
		pin, e := s.PinForward(ctx, generation, historyreports.ForwardFilter{})
		if e != nil {
			return e
		}
		report, e = s.ForwardReport(ctx, pin)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestForwardHandlePreparedUsagePublicationReplayAndFaults(t *testing.T) {
	f := preparedRuntime(t)
	p := f.open(t, nil)
	first := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "forward-logical", c.ID)
	u := forwardMixed(c, "forward-logical", "forward-physical", hookEpoch.Add(time.Second))
	f.h.clock.Add(int64(3 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	h, _ := p.measurement.ReportFeedHead(context.Background())
	ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == h.Through && v.Latest != first.Latest
	})
	r := forwardReportAt(t, p, ready.Latest)
	if r.Totals.UniqueAttempts == nil || *r.Totals.UniqueAttempts != 1 || r.Totals.SelectionValue == nil || r.Totals.SelectionValue.Low != 725e7 || r.Totals.SelectionValue.High != 725e7 || r.Capacity != "unavailable" || p.cfg.Routing.Mode == "on" {
		t.Fatal(r.Totals, r.Capacity)
	}
	var savedJSON, savedCSV bytes.Buffer
	var cursor string
	if e := p.WithForwardReports(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
		if e := s.ExportForwardJSON(ctx, r.Pin, &savedJSON); e != nil {
			return e
		}
		var e error
		cursor, e = s.ExportForwardCSV(ctx, r.Pin, 25, "", &savedCSV)
		return e
	}); e != nil || cursor != "" {
		t.Fatal(e, cursor)
	}
	// Immutable recorded facts are grouped by a new append-only profile revision.
	if e := p.WithForwardReports(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
		_, e := s.EditProfiles(ctx, 0, historyreports.ForwardProfileEdit{Profile: historyreports.ForwardProfile{ID: "forward-profile", Provider: c.Provider, Memberships: []historyreports.ForwardMembership{{Account: historyreports.ForwardAccountRef(f.source, c.Provider, c.Account), Start: hookEpoch.UnixMilli()}}}})
		return e
	}); e != nil {
		t.Fatal(e)
	}
	f.ticks <- f.h.now()
	profile := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Latest != ready.Latest })
	if got := forwardReportAt(t, p, profile.Latest); got.Profiles["forward-profile"].Rows != 1 {
		t.Fatal(got.Profiles)
	}
	// Existing background checkpoint failure is simulated only through the fake
	// host. The new worker itself must never issue that RPC or warm a chat.
	f.h.mu.Lock()
	before := len(f.h.checkpointCalls)
	f.h.mode = "decode"
	f.h.mu.Unlock()
	if before != 0 {
		t.Fatal("publisher invoked checkpoint", before)
	}
	p.refreshSourceCoverage()
	p.WakeForward()
	faulted := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Latest != profile.Latest
	})
	if got := forwardReportAt(t, p, faulted.Latest); got.Totals.CoverageReasons["local_source_fault"] != 1 || got.Capacity != "unavailable" || got.Totals.SelectionValue.Low != 725e7 {
		t.Fatal(got)
	}
	// Alias/unknown model stays recorded, unvalued and sanitized.
	admitCoverage(t, p, "alias-logical", c.ID)
	alias := forwardMixed(c, "alias-logical", "alias-physical", f.h.now())
	alias["Model"] = "client-only-alias"
	alias["APIKey"] = "synthetic-private-key"
	f.h.clock.Add(int64(2 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, alias)
	h, _ = p.measurement.ReportFeedHead(context.Background())
	aliasStatus := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == h.Through && v.Latest != faulted.Latest
	})
	got := forwardReportAt(t, p, aliasStatus.Latest)
	if got.Totals.Rows != 2 || got.Totals.SelectionValue != nil || got.Totals.ValuedCount != 1 {
		t.Fatal(got.Totals)
	}
	invoke(t, p, pluginabi.MethodPluginShutdown, struct{}{})
	if p.ForwardStatus().State != "stopped" {
		t.Fatal(p.ForwardStatus())
	}
	p.Close()
	p = f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Projected == h.Through })
	var replayJSON, replayCSV bytes.Buffer
	if e := p.WithForwardReports(context.Background(), func(ctx context.Context, s *historyreports.ForwardStore) error {
		if e := s.ExportForwardJSON(ctx, r.Pin, &replayJSON); e != nil {
			return e
		}
		_, e := s.ExportForwardCSV(ctx, r.Pin, 25, "", &replayCSV)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(savedJSON.Bytes(), replayJSON.Bytes()) || !bytes.Equal(savedCSV.Bytes(), replayCSV.Bytes()) {
		t.Fatal("old pin/profile/proof changed on reopen")
	}
	if strings.Contains(replayJSON.String()+replayCSV.String(), "synthetic-private-key") {
		t.Fatal("raw input leaked")
	}
}

func TestForwardHandleFeedRollbackAndPostPrimaryPhysicalFailure(t *testing.T) {
	for _, physical := range []bool{false, true} {
		t.Run(map[bool]string{false: "atomic-feed", true: "post-primary-physical"}[physical], func(t *testing.T) {
			f := preparedRuntime(t)
			p := f.open(t, nil)
			initial := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
			c := p.creds[f.c.ID]
			admitCoverage(t, p, "logical", c.ID)
			table := "measurement_report_feed"
			if physical {
				table = "measurement_physical_records"
			}
			restore := rejectSQLInsert(t, p.db, table)
			u := forwardMixed(c, "logical", "failed-callback", hookEpoch.Add(time.Second))
			se := u["SourceEvidence"].(pluginapi.UsageSourceEvidence)
			se.Physical = &pluginapi.UsagePhysicalDispatch{Version: 1, AdmissionSequence: 11, DispatchSequence: 12, StartedAt: se.UpstreamStartedAt.Add(time.Nanosecond)}
			u["SourceEvidence"] = se
			f.h.clock.Add(int64(3 * time.Second))
			_, e := p.Handle(pluginabi.MethodUsageHandle, hookRaw(t, u))
			restore()
			if physical {
				// A physical write after the primary record cannot fail the hook: the
				// record is kept and physical proof turns off for this process.
				if e != nil || p.coverage.PhysicalError() == nil {
					t.Fatal("physical failure reached the hook or went unreported", e)
				}
			} else if e == nil || strings.Contains(e.Error(), "not authorized") {
				t.Fatal("false acknowledgement/raw SQL", e)
			}
			expected := 0
			if physical {
				expected = 1
			}
			for _, table := range []string{"measurement_records", "measurement_record_revisions", "measurement_report_feed"} {
				if n := hookCount(t, p, `SELECT count(*) FROM `+table); n != expected {
					t.Fatal(table, n, expected)
				}
			}
			h, _ := p.measurement.ReportFeedHead(context.Background())
			ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
				return v.State == "ready" && !v.Active && v.Latest != initial.Latest && v.Projected == h.Through
			})
			r := forwardReportAt(t, p, ready.Latest)
			if r.Totals.Rows != int64(expected) || r.Capacity != "unavailable" || p.cfg.Routing.Mode == "on" {
				t.Fatal(r)
			}
			// The kept row is valued and stays unproven; the physical failure no
			// longer faults request coverage.
			if physical && (r.Totals.SelectionValue == nil || r.Totals.SelectionValue.Low != 725e7 || r.Totals.CoverageReasons["physical_coverage_unverified"] != 1 || r.Totals.CoverageReasons["local_source_fault"] != 0) {
				t.Fatal(r.Totals)
			}
		})
	}
}

func TestForwardConfiguredPathsRefuseCreationAliasesAndLiveChanges(t *testing.T) {
	f := preparedRuntime(t)
	for _, mode := range []string{"missing", "empty", "same-state", "hardlink-state", "symlink", "wrong-book"} {
		t.Run(mode, func(t *testing.T) {
			bad := f
			dir := t.TempDir()
			switch mode {
			case "missing":
				bad.path = filepath.Join(dir, "missing.sqlite")
			case "empty":
				bad.path = filepath.Join(dir, "empty.sqlite")
				if e := os.WriteFile(bad.path, nil, 0600); e != nil {
					t.Fatal(e)
				}
			case "same-state":
				bad.path = f.state
			case "hardlink-state":
				bad.path = filepath.Join(dir, "alias.sqlite")
				if e := os.Link(f.state, bad.path); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				bad.path = filepath.Join(dir, "alias.sqlite")
				if e := os.Symlink(f.path, bad.path); e != nil {
					t.Fatal(e)
				}
			case "wrong-book":
				bad.book = strings.Repeat("0", 64)
			}
			p := New(nil)
			defer p.Close()
			if _, e := p.Handle(pluginabi.MethodPluginRegister, hookRaw(t, map[string]any{"config_yaml": bad.config()})); e == nil || strings.Contains(e.Error(), bad.path) {
				t.Fatal("configured failure not closed/safe", e)
			}
			if mode == "missing" {
				if _, e := os.Stat(bad.path); !os.IsNotExist(e) {
					t.Fatal("runtime created missing path", e)
				}
			}
		})
	}
	p := f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" })
	changed := f
	changed.book = strings.Repeat("a", 64)
	if _, e := p.Handle(pluginabi.MethodPluginReconfigure, hookRaw(t, map[string]any{"config_yaml": changed.config()})); e == nil {
		t.Fatal("live reference mutation accepted")
	}
}

func TestForwardHeldSealDoesNotBlockUsageAndShutdownJoins(t *testing.T) {
	f := preparedRuntime(t)
	entered := make(chan struct{})
	var once sync.Once
	p := f.open(t, func(o *forwardOptions) {
		o.seal = func(ctx context.Context, _ *historyreports.ForwardStore, _ historyreports.ForwardSeal) (historyreports.ForwardPin, error) {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return historyreports.ForwardPin{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "live", c.ID)
	u := forwardMixed(c, "live", "while-seal-held", hookEpoch)
	f.h.clock.Add(int64(2 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	if n := hookCount(t, p, `SELECT count(*) FROM measurement_records`); n != 1 {
		t.Fatal("held publisher dropped ingestion", n)
	}
	if status := p.ForwardStatus(); !status.Active || status.Phase != "seal" {
		t.Fatal(status)
	}
	shutdown := make(chan error, 1)
	go func() { _, e := p.Handle(pluginabi.MethodPluginQuiesce, []byte(`{}`)); shutdown <- e }()
	select {
	case e := <-shutdown:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("shutdown failed to join cancelled seal")
	}
	if e := p.WithForwardReports(ctx, func(context.Context, *historyreports.ForwardStore) error { return nil }); !errors.Is(e, ErrForwardUnavailable) {
		t.Fatal(e)
	}
	p.Close()
	if _, e := p.forward.store.Status(ctx); e == nil {
		t.Fatal("store not closed")
	}
	p = f.open(t, nil)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	if r := forwardReportAt(t, p, ""); r.Totals.Rows != 1 || r.Totals.SelectionValue.Low != 725e7 {
		t.Fatal(r.Totals)
	}
}
