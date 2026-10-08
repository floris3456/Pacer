package pacer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func openStartupCatalogPlugin(t *testing.T, h *coverageHostFixture, catalog **startupCatalog, indexed *bool) *Plugin {
	t.Helper()
	host := func(method string, input any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return h.call(method, input)
		}
		files := append([]pluginapi.HostAuthFileEntry{}, h.files...)
		if !*indexed {
			for i := range files {
				files[i].ID = ""
				files[i].Name = "dummy-file"
				files[i].AuthIndex = ""
			}
		}
		return json.Marshal(map[string]any{"files": files, "catalog": *catalog})
	}
	p := New(host)
	p.now = h.now
	p.workerOnce.Do(func() {})
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\nstate_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\nrouting:\n  mode: off\n")})
	t.Cleanup(p.Close)
	return p
}

func TestPacerStartupDefersOnlyExplicitPreEnrollment(t *testing.T) {
	h := newCoverageHost(2)
	indexed := false
	catalog := &startupCatalog{Version: 1, Domain: "cpa.auth.disk", State: "uninitialized", Listener: "registering"}
	p := openStartupCatalogPlugin(t, h, &catalog, &indexed)
	checkWaiting := func() {
		t.Helper()
		p.refreshSourceCoverage()
		if p.coverageStats.Errors != 0 || len(p.creds) != 0 || len(p.coverageGroups) != 0 || len(h.checkpointCalls) != 0 || hookCount(t, p, `SELECT count(*) FROM measurement_physical_faults`) != 0 || hookCount(t, p, `SELECT count(*) FROM measurement_physical_cuts`) != 0 {
			t.Fatal("genuine startup became fault, enrollment or proof", p.coverageStats)
		}
	}
	checkWaiting()
	catalog = &startupCatalog{Version: 1, Domain: "cpa.auth.runtime", State: "loading", Generation: 1, Listener: "installing"}
	p.discover(true)
	checkWaiting()
	indexed = true
	catalog.State = "ready"
	p.discover(true)
	checkWaiting()
	catalog.Listener = "ready"
	p.discover(true)
	if p.coverageStats.Errors != 0 || len(p.creds) != 2 || len(p.coverageGroups) != 2 {
		t.Fatal("runtime listener did not enroll", p.coverageStats)
	}
	for _, c := range p.creds {
		m, err := p.measurement.SourceMembership(context.Background(), c.CoverageMembership)
		if err != nil || m.Complete {
			t.Fatal("runtime readiness claimed complete billing", m, err)
		}
	}
	// Later missing indices are genuine loss, even if the startup disk view was
	// incomplete in exactly this way. A ready observation cannot reset the gap.
	indexed = false
	p.discover(true)
	if p.coverageStats.Errors == 0 || hookCount(t, p, `SELECT count(*) FROM measurement_physical_faults`) == 0 {
		t.Fatal("mid-run identity loss was skipped")
	}
	indexed = true
	p.discover(true)
	if hookCount(t, p, `SELECT count(*) FROM measurement_physical_faults`) == 0 {
		t.Fatal("later ready inventory healed prior faults")
	}
}

func TestPacerStartupUnreadyRegressionAndMalformedCatalogFault(t *testing.T) {
	for _, mode := range []string{"legacy-missing", "failed", "unknown-version", "retired", "later-loading", "empty-ready-then-loss"} {
		t.Run(mode, func(t *testing.T) {
			h := newCoverageHost(1)
			indexed := false
			c := &startupCatalog{Version: 1, Domain: "cpa.auth.runtime", State: "ready", Generation: 1, Listener: "ready"}
			switch mode {
			case "legacy-missing":
				c = nil
			case "failed":
				c.State = "failed"
			case "unknown-version":
				c.Version = 2
			case "retired":
				c.Listener = "retired"
			case "later-loading":
				indexed = true
			case "empty-ready-then-loss":
				indexed = true
				h.files = []pluginapi.HostAuthFileEntry{}
			}
			p := openStartupCatalogPlugin(t, h, &c, &indexed)
			if mode == "later-loading" || mode == "empty-ready-then-loss" {
				c.State = "loading"
				c.Generation = 2
				p.discover(true)
			}
			if p.coverageStats.Errors == 0 || hookCount(t, p, `SELECT count(*) FROM measurement_physical_faults`) == 0 {
				t.Fatal("unavailable catalog was treated as harmless startup", mode)
			}
		})
	}
}

func TestPacerStartupWarmupPrecedesSelectedQuiescentInterval(t *testing.T) {
	h := newCoverageHost(1)
	indexed := false
	catalog := &startupCatalog{Version: 1, Domain: "cpa.auth.disk", State: "uninitialized", Listener: "registering"}
	p := openStartupCatalogPlugin(t, h, &catalog, &indexed)
	indexed = true
	catalog = &startupCatalog{Version: 1, Domain: "cpa.auth.runtime", State: "ready", Generation: 1, Listener: "ready"}
	p.discover(true)
	c := p.creds["auth-000"]
	admitCoverage(t, p, "physical-logical", c.ID)
	invoke(t, p, pluginabi.MethodUsageHandle, physicalHookUsage(c, "warmup-before-lower"))
	lowerAt := hookEpoch.Add(3 * time.Second)
	h.clock.Store(lowerAt.UnixNano())
	response := func(seq uint64, initial bool) pluginapi.HostUsageCheckpointResponse {
		from, through := hookEpoch.Add(-time.Hour), h.now()
		r := pluginapi.HostUsageCheckpointResponse{Scope: sourceScope(c), Coverage: "known", ManagerIncarnationID: "manager-test", IncarnationID: "sink-test", Sequence: seq, From: &from, Through: &through, Gaps: []pluginapi.UsageDeliveryGap{}}
		r.Physical = &pluginapi.UsagePhysicalCheckpoint{Version: 1, Domain: pluginapi.ClaudePhysicalProofDomain, Coverage: "known", FromCut: &pluginapi.UsagePhysicalCut{Sequence: 20, At: lowerAt}, ThroughCut: &pluginapi.UsagePhysicalCut{Sequence: seq, At: through}}
		if initial {
			r.Physical.Coverage = "unknown"
			r.Physical.Reason = "first_quiescent_cut"
			r.Physical.ThroughCut = nil
		}
		return r
	}
	if err := p.coverage.ApplyCheckpoint(context.Background(), c.CoverageMembership, sourceScope(c), response(20, true)); err != nil {
		t.Fatal(err)
	}
	if hookCount(t, p, `SELECT count(*) FROM measurement_physical_proofs`) != 0 {
		t.Fatal("initial cut included warmup as proven history")
	}
	admitCoverage(t, p, "after-lower", c.ID)
	u := hookUsage(c, "after-lower", "selected-after-lower", lowerAt.Add(time.Second))
	e := u["SourceEvidence"].(pluginapi.UsageSourceEvidence)
	e.Physical = &pluginapi.UsagePhysicalDispatch{Version: 1, AdmissionSequence: 31, DispatchSequence: 32, StartedAt: e.UpstreamStartedAt.Add(time.Nanosecond)}
	u["SourceEvidence"] = e
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	h.clock.Store(lowerAt.Add(3 * time.Second).UnixNano())
	if err := p.coverage.ApplyCheckpoint(context.Background(), c.CoverageMembership, sourceScope(c), response(40, false)); err != nil {
		t.Fatal(err)
	}
	selection := physicalHookSelection(t, p, c, h.now())
	selection.From.Sequence = 20
	selection.From.At = lowerAt
	selection.Through.Sequence = 40
	got, err := p.coverage.SelectPhysical(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	selected := hookRecord(t, p, "selected-after-lower")
	warmup := hookRecord(t, p, "warmup-before-lower")
	if !got.Known || len(got.Facts) != 1 || got.Facts[0].EventID != selected.EventID || got.Facts[0].EventID == warmup.EventID || got.Facts[0].DispatchSequence != 32 {
		t.Fatal("warmup/selected sequence join", got)
	}
	snapshot, err := p.coverage.Snapshot(context.Background(), c.CoverageMembership, c.Provider, c.Account, lowerAt, h.now())
	if err != nil || snapshot.Known || snapshot.PhysicalCarryInProven || selected.Native.BillingMappingVerified {
		t.Fatal("causal startup promoted numeric coverage", snapshot, err)
	}
	indexed = false
	p.discover(true)
	got, err = p.coverage.SelectPhysical(context.Background(), selection)
	if err != nil || got.Known {
		t.Fatal("mid-run identity loss retained earlier local proof", got, err)
	}
}
