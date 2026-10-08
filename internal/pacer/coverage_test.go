package pacer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var hookEpoch = time.Date(2026, 10, 8, 1, 0, 0, 137, time.UTC)

type coverageHostFixture struct {
	mu               sync.Mutex
	clock            atomic.Int64
	files            []pluginapi.HostAuthFileEntry
	billing          map[string]string
	listFailure      bool
	mode             string
	sequence         uint64
	checkpointCalls  []string
	otherCalls       []string
	plugin           *Plugin
	entered, release chan struct{}
}

func newCoverageHost(n int) *coverageHostFixture {
	h := &coverageHostFixture{billing: map[string]string{}, sequence: 10}
	h.clock.Store(hookEpoch.UnixNano())
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("auth-%03d", i)
		index := fmt.Sprintf("index-%03d", i)
		h.files = append(h.files, pluginapi.HostAuthFileEntry{ID: id, AuthIndex: index, Provider: "claude"})
		h.billing[index] = "billing-" + index
	}
	return h
}
func (h *coverageHostFixture) now() time.Time { return time.Unix(0, h.clock.Load()).UTC() }
func (h *coverageHostFixture) call(method string, input any) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "host.auth.list":
		if h.listFailure {
			return nil, errors.New("dummy discovery failure")
		}
		return json.Marshal(map[string]any{"files": h.files})
	case "host.auth.get":
		return json.Marshal(map[string]any{"json": map[string]string{"account_uuid": h.billing[input.(map[string]string)["auth_index"]]}})
	case pluginabi.MethodHostUsageCheckpoint:
		s := input.(pluginapi.HostUsageCheckpointRequest).Scope
		h.checkpointCalls = append(h.checkpointCalls, s.AuthID)
		if h.plugin != nil {
			if !h.plugin.mu.TryLock() {
				return nil, errors.New("checkpoint called under plugin mutex")
			}
			h.plugin.mu.Unlock()
		}
		if h.entered != nil {
			entered, release := h.entered, h.release
			h.entered = nil
			close(entered)
			h.mu.Unlock()
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			h.mu.Lock()
		}
		if h.mode == "error" {
			return nil, errors.New("dummy missing callback")
		}
		if h.mode == "decode" {
			return []byte(`{`), nil
		}
		from, through := hookEpoch.Add(-time.Hour), h.now()
		r := pluginapi.HostUsageCheckpointResponse{Scope: s, Coverage: "known", ManagerIncarnationID: "manager-test", IncarnationID: "sink-test", Sequence: h.sequence, From: &from, Through: &through, Gaps: []pluginapi.UsageDeliveryGap{}}
		if h.mode == "gap" {
			r.Coverage = "unknown"
			r.Through = nil
			r.GapCount = 20
			r.GapsTruncated = true
			r.InvalidFrom = &from
			r.Gaps = []pluginapi.UsageDeliveryGap{{Sequence: 1, Reason: "untracked_publication", From: &from, ObservedAt: h.now()}}
		}
		if h.mode == "foreign" {
			r.Scope.AuthIndex = "foreign-index"
		}
		return json.Marshal(r)
	default:
		h.otherCalls = append(h.otherCalls, method)
		return []byte(`{}`), nil
	}
}

func openCoveragePlugin(t *testing.T, h *coverageHostFixture, path string) *Plugin {
	t.Helper()
	p := New(h.call)
	p.now = h.now
	// Drive the very same background entry deterministically, not a wall timer.
	p.workerOnce.Do(func() {})
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + path + "\nrouting:\n  mode: off\n")})
	h.mu.Lock()
	h.plugin = p
	h.mu.Unlock()
	t.Cleanup(p.Close)
	return p
}
func admitCoverage(t *testing.T, p *Plugin, id, auth string) {
	t.Helper()
	r := pluginapi.RequestInterceptRequest{RequestID: id, Model: "claude-opus-5-5", Metadata: map[string]any{"caller_scope": "dummy-caller", "selected_auth_id": auth}, Headers: http.Header{}}
	before := invoke(t, p, pluginabi.MethodRequestInterceptBefore, r).(pluginapi.RequestInterceptResponse)
	if before.Terminate {
		t.Fatal("before refused")
	}
	r.Headers = before.Headers
	invoke(t, p, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{Provider: "claude", Model: r.Model, Options: pluginapi.SchedulerOptions{Headers: r.Headers}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: auth, Provider: "claude"}}})
	if after := invoke(t, p, pluginabi.MethodRequestInterceptAfter, r).(pluginapi.RequestInterceptResponse); after.Terminate {
		t.Fatal(string(after.ResponseBody))
	}
}
func hookUsage(c credential, logical, physical string, requested time.Time) map[string]any {
	start := requested.Add(time.Nanosecond)
	return map[string]any{"Provider": c.Provider, "AuthID": c.ID, "AuthIndex": c.Index, "LifecycleRequestID": logical, "RequestID": physical, "RequestedAt": requested, "Latency": int64(time.Second), "Failed": false, "Model": "claude-opus-5-5", "ExecutorType": "ClaudeExecutor", "AccountingVersion": 2,
		"SourceEvidence": pluginapi.UsageSourceEvidence{Scope: sourceScope(c), ManagerIncarnationID: "manager-test", Tracked: true, RequestedAt: requested, UpstreamStartedAt: &start, FinalUsageAt: requested.Add(time.Second + time.Nanosecond)},
		"Detail":         map[string]any{"CacheCreation": map[string]any{"total_tokens": 100, "ephemeral_5m_input_tokens": 25, "ephemeral_1h_input_tokens": 75, "unknown_tokens": 0, "quality": "complete"}},
		"TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 400, "input": map[string]any{"total_tokens": 400, "uncached_tokens": 100, "cache_read_tokens": 200, "cache_write_tokens": 100}, "output": map[string]any{"total_tokens": 0, "non_reasoning_tokens": 0, "reasoning_tokens": 0}, "unclassified_tokens": 0}}
}
func hookRaw(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func hookCount(t *testing.T, p *Plugin, query string, args ...any) int {
	t.Helper()
	var n int
	if err := p.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func hookRecord(t *testing.T, p *Plugin, physical string) measurement.Record {
	t.Helper()
	var body []byte
	if err := p.db.QueryRow(`SELECT body FROM measurement_records WHERE attempt_id=?`, physical).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var r measurement.Record
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCoverageHooksUnmanagedAcknowledgedCaptureAndPointInventory(t *testing.T) {
	h := newCoverageHost(2)
	h.billing["index-001"] = h.billing["index-000"]
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	c := p.creds["auth-000"]
	if !c.AccountVerified || !c.ExactIdentity || c.CoverageMembership == "" {
		t.Fatal(c)
	}
	m, err := p.measurement.SourceMembership(context.Background(), c.CoverageMembership)
	if err != nil {
		t.Fatal(err)
	}
	if m.Complete || !m.Verified || !m.From.Equal(m.Through) || len(m.Members) != 2 {
		t.Fatalf("invented interval membership: %+v", m)
	}
	admitCoverage(t, p, "logical", c.ID)
	u := hookUsage(c, "logical", "unmanaged", hookEpoch)
	h.clock.Add(int64(2 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	r := hookRecord(t, p, "unmanaged")
	if r.StartTimeBasis != "upstream" || !r.StartedAt.Equal(hookEpoch.Add(time.Nanosecond)) || r.Native.SourceEvidence == nil || r.Native.SourceMembershipID != m.ID || r.Native.BillingMappingVerified || r.Policy.SelectedTTL != "" {
		t.Fatalf("unmanaged provenance: %+v", r)
	}
	if r.Usage.CacheCreate5m == nil || *r.Usage.CacheCreate5m != 25 || r.Usage.CacheCreate1h == nil || *r.Usage.CacheCreate1h != 75 {
		t.Fatal(r.Usage)
	}
	if hookCount(t, p, `SELECT count(*) FROM measurement_records`) != 1 {
		t.Fatal("duplicate callback created duplicate usage")
	}
	h.mu.Lock()
	calls := len(h.checkpointCalls)
	h.mu.Unlock()
	if calls != 0 {
		t.Fatal("request/usage path called checkpoint")
	}
	p.refreshMeasurements()
	if hookCount(t, p, `SELECT count(*) FROM measurement_delivery_runs`) != 0 {
		t.Fatal("native opened global run")
	}
	if hookCount(t, p, `SELECT count(*) FROM measurement_source_runs`) != 2 {
		t.Fatal("background didn't poll every alias")
	}
	for _, report := range p.measurements {
		if report.Coverage.Observed || report.FiveHour.Known || !strings.Contains(strings.Join(report.Coverage.Reasons, ","), "incomplete_account_membership") {
			t.Fatal(report.Coverage)
		}
	}
	before := hookCount(t, p, `SELECT count(*) FROM measurement_source_memberships`)
	p.discover(true)
	if hookCount(t, p, `SELECT count(*) FROM measurement_source_memberships`) != before {
		t.Fatal("unchanged poll grew membership history")
	}
}

func TestCoverageHooksMissingCorrelationAndDBFailureAreAcknowledgmentErrors(t *testing.T) {
	for _, mode := range []string{"no-lifecycle", "no-provider", "uncaptured", "bad-index", "record-write", "gap-write", "invalid-headers"} {
		t.Run(mode, func(t *testing.T) {
			h := newCoverageHost(1)
			p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
			c := p.creds["auth-000"]
			admitCoverage(t, p, "logical", c.ID)
			u := hookUsage(c, "logical", "physical", hookEpoch)
			h.clock.Add(int64(2 * time.Second))
			var restore func()
			switch mode {
			case "no-lifecycle":
				delete(u, "LifecycleRequestID")
			case "no-provider":
				delete(u, "Provider")
			case "uncaptured":
				u["LifecycleRequestID"] = "not-captured"
			case "bad-index":
				u["AuthIndex"] = "different-index"
			case "record-write":
				restore = rejectSQLInsert(t, p.db, "measurement_records")
			case "gap-write":
				restore = rejectSQLInsert(t, p.db, "measurement_source_faults")
			case "invalid-headers":
				u["ResponseHeaders"] = 123
			}
			_, err := p.Handle(pluginabi.MethodUsageHandle, hookRaw(t, u))
			if restore != nil {
				restore()
			}
			if err == nil {
				t.Fatal("unrecorded/invalid callback acknowledged")
			}
			if strings.Contains(err.Error(), c.ID) {
				t.Fatal("callback error leaks identity")
			}
			if mode == "gap-write" {
				p.refreshMeasurements()
				if hookCount(t, p, `SELECT poisoned FROM measurement_source_consumers WHERE id=?`, p.coverage.ID()) != 1 {
					t.Fatal("write failure latch not persisted")
				}
			} else if hookCount(t, p, `SELECT count(*) FROM measurement_source_faults`) == 0 {
				t.Fatal("missing durable invalidation")
			}
			if len(p.measurements) > 0 {
				for _, r := range p.measurements {
					if r.Coverage.Observed {
						t.Fatal("error retained coverage")
					}
				}
			}
		})
	}
}

func TestCoverageHooksRestartRetainsCaptureAndLegacyRowsWithoutRemapping(t *testing.T) {
	h := newCoverageHost(1)
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := openCoveragePlugin(t, h, path)
	c := p.creds["auth-000"]
	source := p.measurementSource
	oldConsumer := p.coverage.ID()
	admitCoverage(t, p, "logical", c.ID)
	u := hookUsage(c, "logical", "delayed", hookEpoch)
	p.Close()
	h.mu.Lock()
	h.billing[c.Index] = "replaced-account"
	h.mu.Unlock()
	// Identical clock is a real reload possibility. It must poison proof while
	// still opening the sink and accepting captured usage.
	p = openCoveragePlugin(t, h, path)
	if p.coverage.ID() == oldConsumer || p.measurementSource != source {
		t.Fatal("consumer/source incarnation identity")
	}
	if hookCount(t, p, `SELECT poisoned FROM measurement_source_consumers WHERE id=?`, p.coverage.ID()) != 1 {
		t.Fatal("equal-time reload borrowed proof")
	}
	h.clock.Add(int64(3 * time.Second))
	u["ResponseHeaders"] = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.25"}, "Anthropic-Ratelimit-Unified-5h-Reset": {"1791507600"}}
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	r := hookRecord(t, p, "delayed")
	if r.Attribution.AccountID != c.Account || r.Native.SourceMembershipID != c.CoverageMembership {
		t.Fatal("delayed callback remapped", r.Attribution)
	}
	if rows, err := p.observer.ReadWindows(c.Account); err != nil || len(rows) == 0 {
		t.Fatal("delayed headers lost captured account", rows, err)
	}
	if rows, err := p.observer.ReadWindows(p.creds[c.ID].Account); err != nil || len(rows) != 0 {
		t.Fatal("delayed headers remapped current inventory", rows, err)
	}
	// Existing bare NativeCredential captures are readable, never upgraded to
	// verified metadata by current inventory or by a later repeated capture.
	if _, err := p.db.Exec(`DELETE FROM pacer_usage_capture_metadata WHERE request_id='logical'`); err != nil {
		t.Fatal(err)
	}
	u["RequestID"] = "legacy-row"
	delete(u, "SourceEvidence")
	delete(u, "ResponseHeaders")
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	r = hookRecord(t, p, "legacy-row")
	if r.Attribution.AccountID != c.Account || r.Native.SourceMembershipID != "" || r.Native.BillingMappingVerified {
		t.Fatal(r.Native)
	}
}

func TestCoverageHooksDiscoveryFailuresAliasesAndCredentialChanges(t *testing.T) {
	h := newCoverageHost(2)
	h.billing["index-001"] = h.billing["index-000"]
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	old := p.creds["auth-000"]
	admitCoverage(t, p, "original", old.ID)
	h.clock.Add(int64(time.Second))
	h.mu.Lock()
	h.files = h.files[:1]
	h.mu.Unlock()
	p.discover(true)
	changed := p.creds[old.ID]
	if changed.CoverageMembership == old.CoverageMembership {
		t.Fatal("alias removal reused membership")
	}
	if err := p.captureUsageCredential("original", changed); err != nil {
		t.Fatal("same credential snapshot retry should retain first metadata", err)
	}
	var body []byte
	if err := p.db.QueryRow(`SELECT body FROM pacer_usage_capture_metadata WHERE request_id='original'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var meta usageCaptureMetadata
	if err := json.Unmarshal(body, &meta); err != nil || meta.MembershipID != old.CoverageMembership {
		t.Fatal("capture snapshot changed", meta, err)
	}
	h.clock.Add(int64(time.Second))
	h.mu.Lock()
	h.files[0].AuthIndex = "rotated-index"
	h.billing["rotated-index"] = h.billing[old.Index]
	h.mu.Unlock()
	p.discover(true)
	if err := p.captureUsageCredential("original", p.creds[old.ID]); err == nil {
		t.Fatal("same request rebound immutable qualifier")
	}
	h.mu.Lock()
	h.listFailure = true
	h.mu.Unlock()
	p.discover(true)
	if len(p.coverageGroups) != 0 || p.creds[old.ID].AccountVerified || p.creds[old.ID].CoverageMembership != "" {
		t.Fatal("failed discovery retained verified capture inventory")
	}
	if hookCount(t, p, `SELECT count(*) FROM measurement_source_faults WHERE account_id=''`) == 0 {
		t.Fatal("discovery failure not durable")
	}
}

func TestCoverageHooksFallbackAndGatewayNeverClaimAccountMembership(t *testing.T) {
	h := newCoverageHost(2)
	h.billing["index-000"] = ""
	h.files[1].BaseURL = "https://gateway.invalid"
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	c := p.creds["auth-000"]
	if c.AccountVerified {
		t.Fatal("credential fallback verified")
	}
	m, err := p.measurement.SourceMembership(context.Background(), c.CoverageMembership)
	if err != nil || m.Verified || m.Complete {
		t.Fatal(m, err)
	}
	if _, ok := p.creds["auth-001"]; ok {
		t.Fatal("gateway acquired first-party identity")
	}
	admitCoverage(t, p, "gateway", "auth-001")
	u := hookUsage(credential{ID: "auth-001", Index: "index-001", Provider: "claude"}, "gateway", "gateway-physical", hookEpoch)
	if _, err = p.Handle(pluginabi.MethodUsageHandle, hookRaw(t, u)); err == nil {
		t.Fatal("uncaptured gateway silently acknowledged")
	}
}

func TestCoverageHooksBackgroundBoundedSortedAndOutsideMutex(t *testing.T) {
	h := newCoverageHost(12)
	for index := range h.billing {
		h.billing[index] = "one-billing-account"
	}
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	admitCoverage(t, p, "logical", "auth-000")
	if len(h.checkpointCalls) > 0 {
		t.Fatal("request checkpoint RPC")
	}
	h.clock.Add(int64(time.Second))
	p.refreshSourceCoverage()
	first := append([]string(nil), h.checkpointCalls...)
	if len(first) != maxCoverageCalls || !sort.StringsAreSorted(first) {
		t.Fatal(first)
	}
	p.refreshSourceCoverage()
	seen := map[string]bool{}
	for _, id := range h.checkpointCalls {
		seen[id] = true
	}
	if len(seen) != 12 {
		t.Fatal("round-robin starved a tuple", h.checkpointCalls)
	}
	if p.coverageStats.Errors != 0 {
		t.Fatal("RPC used plugin mutex", p.coverageStats)
	}
	for _, method := range h.otherCalls {
		if method == "host.usage.delivery" || strings.HasPrefix(method, "host.http") {
			t.Fatal("unexpected host fallback", method)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.entered = entered
	h.release = release
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { p.refreshSourceCoverage(); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter")
	}
	// Checkpoint work must not hold p.mu or the consumer lock while in Host.
	admitCoverage(t, p, "while-checkpoint", "auth-000")
	u := hookUsage(p.creds["auth-000"], "while-checkpoint", "concurrent", hookEpoch)
	h.clock.Add(int64(time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker stuck")
	}
}

func TestCoverageHooksStaleGapAndRPCFailuresAreSticky(t *testing.T) {
	for _, mode := range []string{"gap", "error", "decode", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			h := newCoverageHost(1)
			p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
			c := p.creds["auth-000"]
			h.clock.Add(int64(2 * time.Second))
			p.refreshSourceCoverage()
			h.mu.Lock()
			h.mode = mode
			h.sequence = 9
			h.mu.Unlock()
			p.refreshSourceCoverage()
			if mode == "gap" {
				if p.coverageStats.Stale != 1 || hookCount(t, p, `SELECT count(*) FROM measurement_source_gaps`) != 1 {
					t.Fatal("stale gap discarded", p.coverageStats)
				}
				var from int64
				if err := p.db.QueryRow(`SELECT invalid_from FROM measurement_source_gaps`).Scan(&from); err != nil || from != hookEpoch.Add(-time.Hour).UnixNano() {
					t.Fatal("truncated invalid_from lost", from, err)
				}
			} else if hookCount(t, p, `SELECT count(*) FROM measurement_source_faults`) == 0 {
				t.Fatal("host failure not durable")
			}
			h.mu.Lock()
			h.mode = ""
			h.sequence = 11
			h.mu.Unlock()
			p.refreshSourceCoverage()
			s, err := p.coverage.Snapshot(context.Background(), c.CoverageMembership, "claude", c.Account, hookEpoch, hookEpoch.Add(time.Second))
			if err != nil || s.Known || !s.HasGap {
				t.Fatal("success erased gap", s, err)
			}
		})
	}
}

func TestCoverageHooksNativeEmptyQueriesNeverBorrowGlobalRunsOrNeedConsumer(t *testing.T) {
	h := newCoverageHost(1)
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	c := p.creds["auth-000"]
	h.clock.Add(int64(22 * 24 * time.Hour))
	from := h.now().Add(-21 * 24 * time.Hour)
	if err := p.measurement.StartDelivery(context.Background(), "legacy-global", from.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := p.measurement.Checkpoint(context.Background(), "legacy-global", h.now(), false); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		if missing {
			p.mu.Lock()
			p.coverage = nil
			p.mu.Unlock()
		}
		p.refreshMeasurements()
		r, ok := p.measurements[c.Account]
		if !ok || r.Coverage.Observed || r.Coverage.Delivery != "cpa-scoped-request-start" || r.FiveHour.Known || r.WarmBurn.Bounded {
			t.Fatal("global proof borrowed", r.Coverage, ok)
		}
	}
}

func TestCoverageHooksConsumerReturnsExactPersistedProjection(t *testing.T) {
	h := newCoverageHost(1)
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	c := p.creds["auth-000"]
	admitCoverage(t, p, "logical", c.ID)
	u := hookUsage(c, "logical", "managed", hookEpoch)
	u["CachePolicy"] = map[string]any{"selected": pluginapi.CachePolicy{Protocol: 1, Scope: pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "installation", SessionID: "session", ProviderRoute: "claude-main"}, ContextEpoch: "epoch", Revision: 1, TTL: "5m", Kind: "primary"}, "effective_ttl": "5m", "breakpoint_count": 1, "dispatched": true, "upstream_started_at": hookEpoch}
	h.clock.Add(int64(2 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	r := hookRecord(t, p, "managed")
	if r.Policy.SelectedTTL != "5m" || r.Usage.CacheCreate5m == nil || *r.Usage.CacheCreate5m != 25 || r.Usage.CacheCreate1h == nil || *r.Usage.CacheCreate1h != 75 {
		t.Fatal("writes reclassified", r.Usage)
	}
	// The optional projection already observed this authoritative identity. Its
	// same-record replay must be idempotent, including captured native metadata.
	if _, err := p.policyEvidence.Observe(context.Background(), r); err != nil {
		t.Fatal("projection identity differs from persisted record", err)
	}
	before := r
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	after := hookRecord(t, p, "managed")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("duplicate changed immutable record")
	}
}

func TestCoverageHooksMissingHostAndDiscoveryOverflowFailClosed(t *testing.T) {
	for _, mode := range []string{"missing-host", "inventory-overflow", "duplicate-tuple", "name-only"} {
		t.Run(mode, func(t *testing.T) {
			h := newCoverageHost(1)
			p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
			switch mode {
			case "missing-host":
				p.Host = nil
				p.refreshSourceCoverage()
			case "inventory-overflow":
				h.mu.Lock()
				for len(h.files) <= maxCoverageInventory {
					h.files = append(h.files, h.files[0])
				}
				h.mu.Unlock()
				p.discover(true)
			case "duplicate-tuple":
				h.mu.Lock()
				h.files = append(h.files, h.files[0])
				h.mu.Unlock()
				p.discover(true)
			case "name-only":
				h.mu.Lock()
				h.files[0].Name = h.files[0].ID
				h.files[0].ID = ""
				h.mu.Unlock()
				p.discover(true)
			}
			if mode != "missing-host" && len(p.coverageGroups) != 0 {
				t.Fatal("invalid identity acquired source membership")
			}
			if hookCount(t, p, `SELECT count(*) FROM measurement_source_faults`) == 0 {
				t.Fatal("failure lacked durable invalidation")
			}
		})
	}
}
