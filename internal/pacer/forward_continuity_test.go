package pacer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// Contract §5 runtime gate (forward-continuity-contract.md §3) through the real
// plugin registration, management wiring and restart.
const (
	continuityRuntimeH     = int64(1790812800000) // frozen fixture H
	continuityRuntimeN0    = int64(1790726400000) // overlap [N0,H) holds 45 of 80 events
	continuityRuntimeBasis = "measurement store first certified source checkpoint (synthetic runtime)"
)

// The retained historical fixture with synthetic CPAMP request IDs, so the
// origin index can link every admitted event (missing_count 0).
func continuityFrozenReader(t *testing.T) *historyreports.Reader {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "frozen.sqlite")
	f, err := os.Open("../historyreports/testdata/historical-golden/historical-fixture.sql.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE analytics_events SET payload=json_set(payload,'$.request_id','synthetic-physical-'||event_id)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(source, 0400); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(snapshot)
	archive, err := history.Open(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	boundary := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hex.EncodeToString(digest[:]), LineageSHA256: strings.Repeat("a", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: continuityRuntimeH, EventsThrough: 240, ObservationsThrough: 65}
	if progress, err := archive.Import(context.Background(), history.ImportOptions{SourcePath: source, Boundary: boundary, BatchSize: 25}); err != nil || !progress.Complete {
		t.Fatal(progress, err)
	}
	reader, err := historyreports.Prepare(context.Background(), archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return reader
}

// Offline preparation only (never a runtime path): base copy, frozen profile
// seed, one origin certificate [0,∞) per legacy account covering the whole
// baseline. Certificates never define N0.
func continuityRuntimeBaseline(t *testing.T, f forwardFixture) (origin string, seeded []string) {
	t.Helper()
	ctx := context.Background()
	r := continuityFrozenReader(t)
	s, err := historyreports.OpenForward(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := s.PrepareForwardBase(ctx, r, measurement.ReportBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SeedFrozenForwardProfiles(ctx, r, base); err != nil {
		t.Fatal(err)
	}
	_, frozen, err := r.ForwardMetadata(ctx)
	if err != nil || len(frozen.Profiles) < 2 {
		t.Fatal("frozen profiles", err)
	}
	for _, p := range frozen.Profiles {
		seeded = append(seeded, p.ID)
	}
	origins := historyreports.ForwardOrigins{BaseDigest: base}
	seen := map[string]bool{}
	for after := ""; ; {
		page, e := r.ReadBaselineEvents(ctx, after, measurement.ReportBudget{})
		if e != nil {
			t.Fatal(e)
		}
		for _, e := range page.Events {
			if seen[e.Event.Account] {
				continue
			}
			seen[e.Event.Account] = true
			origins.Certificates = append(origins.Certificates, historyreports.ForwardOriginCertificate{LegacyAccount: e.Event.Account, Provider: e.Event.Provider, SourceID: f.source, NativeAccount: "native-" + e.Event.Account, NativeCredentialRef: "credential", StartMS: 0, EndMS: 0, EvidenceRef: "synthetic-continuity-v1", ProducerVersion: "synthetic-cpa-v1", ParserVersion: "synthetic-cpamp-physical-id-v1"})
		}
		after = page.Next
		if page.Done {
			break
		}
	}
	if origin, err = s.RegisterForwardOrigins(ctx, origins); err != nil {
		t.Fatal(err)
	}
	return origin, seeded
}

func continuityRuntimeCutover(t *testing.T, f forwardFixture, origin string, n0 int64, declare bool) error {
	t.Helper()
	s, err := historyreports.OpenForward(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.PrepareForwardCutover(context.Background(), origin, n0, continuityRuntimeBasis, declare)
	return err
}

func continuityRuntimeRefuses(t *testing.T, f forwardFixture, why string) {
	t.Helper()
	p := New(nil)
	defer p.Close()
	if _, e := p.Handle(pluginabi.MethodPluginRegister, hookRaw(t, map[string]any{"config_yaml": f.config()})); e == nil || strings.Contains(e.Error(), f.path) {
		t.Error("runtime accepted a baseline store:", why, e)
	}
}

func TestForwardRuntimeAcceptsPreparedCutoverAcrossRestart(t *testing.T) {
	ctx := context.Background()
	f := preparedRuntime(t)
	origin, seeded := continuityRuntimeBaseline(t, f)
	continuityRuntimeRefuses(t, f, "no recorded H/N0 cutover")
	if err := continuityRuntimeCutover(t, f, origin, continuityRuntimeN0, false); err != nil {
		t.Fatal(err)
	}
	p := forwardWiringOpen(t, f, nil, nil)
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	want := historyreports.ForwardCutover{HMS: continuityRuntimeH, EventsThrough: 240, ObservationsThrough: 65, N0MS: continuityRuntimeN0, N0Basis: continuityRuntimeBasis}
	wiredBasis := []byte(`"n0_basis":"` + continuityRuntimeBasis + `"`)
	first := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Latest != "" })
	if first.Cutover == nil || first.Cutover.ForwardCutover != want {
		t.Errorf("runtime status cutover %+v", first.Cutover)
	}
	r := forwardReportAt(t, p, first.Latest)
	if r.Base == nil || r.Totals.Rows != 80 || r.Cutover == nil || r.Cutover.PreCutoverNative != 0 || !reflect.DeepEqual(r.Totals.DedupUnverified, &historyreports.ForwardDedup{UnlinkedBaseline: 45}) {
		t.Errorf("baseline report rows=%d cutover=%+v dedup=%+v", r.Totals.Rows, r.Cutover, r.Totals.DedupUnverified)
	}
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "continuity-logical", c.ID)
	f.h.clock.Add(int64(3 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "continuity-logical", "continuity-physical", hookEpoch.Add(time.Second)))
	head, err := p.measurement.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == head.Through && v.Latest != first.Latest
	})
	lr := forwardReportAt(t, p, live.Latest)
	if lr.Totals.Rows != 81 || lr.Cutover == nil || lr.Totals.DedupUnverified == nil || lr.Totals.DedupUnverified.UnlinkedBaseline != 45 {
		t.Errorf("live usage after H rows=%d dedup=%+v", lr.Totals.Rows, lr.Totals.DedupUnverified)
	}
	statusBody := forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200)
	var wired forwardWiredStatus
	if json.Unmarshal(statusBody.Body, &wired) != nil || wired.Publisher.Cutover == nil || wired.Publisher.Cutover.ForwardCutover != want || !bytes.Contains(statusBody.Body, wiredBasis) {
		t.Error("status route omitted cutover", string(statusBody.Body))
	}
	// Seeded profile IDs are forward copies: the management route changes and
	// archives them as forward revisions; old pins replay unchanged.
	patch := func(body map[string]any, expected string) {
		t.Helper()
		body["expected_revision"] = expected
		req := forwardWiringRequest(t, "profiles", body)
		req.Method = "PATCH"
		forwardWiringCall(t, p, req, 202)
	}
	membership := []map[string]string{{"account": "a", "start_ms": strconv.FormatInt(continuityRuntimeN0, 10)}}
	exportRequest := forwardWiringRequest(t, "export-json", map[string]any{"pin": lr.Pin})
	saved := forwardWiringCall(t, p, exportRequest, 200)
	patch(map[string]any{"profile": map[string]any{"id": seeded[0], "name": "rewritten", "provider": "claude", "memberships": membership}}, "1")
	patch(map[string]any{"archive_id": seeded[1]}, "2")
	patch(map[string]any{"profile": map[string]any{"id": "forward-live", "name": "Forward", "provider": "claude", "memberships": membership}}, "3")
	edited := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Latest != live.Latest && forwardReportAt(t, p, v.Latest).Manifest.ProfileRevision == 4
	})
	if got := forwardReportAt(t, p, edited.Latest); got.Profiles["forward-live"].Rows < 1 || got.Profiles[seeded[0]].Rows < 1 || got.Cutover == nil || got.Cutover.ForwardCutover != want {
		t.Error("forward profile revision not published", got.Profiles["forward-live"], got.Profiles[seeded[0]], got.Cutover)
	}
	// Restart re-runs the gate after the edits and preserves the accepted
	// cutover, its basis, old pins and the recorded status.
	invoke(t, p, pluginabi.MethodPluginShutdown, struct{}{})
	p.Close()
	p = forwardWiringOpen(t, f, nil, nil)
	forwardWiringRegister(t, p, hookRaw(t, forwardWiringRegistration()))
	again := forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == head.Through
	})
	if again.Cutover == nil || again.Cutover.ForwardCutover != want {
		t.Errorf("restart lost cutover %+v", again.Cutover)
	}
	if body := forwardWiringCall(t, p, forwardWiringRequest(t, "status", nil), 200).Body; !bytes.Contains(body, wiredBasis) {
		t.Error("status route lost n0_basis across restart", string(body))
	}
	if replay := forwardWiringCall(t, p, exportRequest, 200); !bytes.Equal(replay.Body, saved.Body) {
		t.Error("cutover pin changed across restart")
	}
}

// Certificates [0,∞) cover the whole baseline; a declared N0 one day after H is
// a real gap that a certificate-derived N0 (0) would have hidden.
func TestForwardRuntimeCoverageGapRequiresDeclaration(t *testing.T) {
	f := preparedRuntime(t)
	n0 := continuityRuntimeH + 24*3600000
	origin, _ := continuityRuntimeBaseline(t, f)
	if err := continuityRuntimeCutover(t, f, origin, n0, false); !errors.Is(err, historyreports.ErrForwardCoverageGap) {
		t.Error("N0 > H prepared without declaration", err)
	}
	continuityRuntimeRefuses(t, f, "N0 > H without a coverage_gap declaration")
	if err := continuityRuntimeCutover(t, f, origin, n0, true); err != nil {
		t.Fatal(err)
	}
	p := f.open(t, nil)
	ready := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active && v.Latest != "" })
	gap := &historyreports.ForwardCoverageGap{FromMS: continuityRuntimeH, ToMS: n0, Accounts: []string{"a", "b", "c", "d", "forecast", "gap"}}
	if ready.Cutover == nil || !reflect.DeepEqual(ready.Cutover.CoverageGap, gap) || ready.Cutover.N0MS != n0 || ready.Cutover.N0Basis != continuityRuntimeBasis {
		t.Errorf("status coverage gap %+v", ready.Cutover)
	}
	if r := forwardReportAt(t, p, ready.Latest); r.Cutover == nil || !reflect.DeepEqual(r.Cutover.CoverageGap, gap) {
		t.Errorf("report coverage gap %+v", r.Cutover)
	}
}

// No runtime source path imports a baseline, seeds profiles or edits origins.
func TestForwardRuntimeHasNoPreparationPath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"PrepareForwardBase", "RegisterForwardOrigins", "PrepareForwardCutover", "SeedFrozenForwardProfiles", "SeedForwardProfiles", "historyreports.OpenForward("} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Errorf("%s references %s", name, forbidden)
			}
		}
	}
}
