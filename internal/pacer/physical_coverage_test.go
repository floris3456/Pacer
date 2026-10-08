package pacer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func physicalHookPlugin(t *testing.T) (*Plugin, *coverageHostFixture, credential) {
	t.Helper()
	h := newCoverageHost(1)
	p := openCoveragePlugin(t, h, filepath.Join(t.TempDir(), "state.sqlite"))
	p.Host = func(method string, input any) (json.RawMessage, error) {
		raw, err := h.call(method, input)
		if err != nil || method != pluginabi.MethodHostUsageCheckpoint {
			return raw, err
		}
		var r pluginapi.HostUsageCheckpointResponse
		if err = json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		r.Physical = &pluginapi.UsagePhysicalCheckpoint{Version: 1, Domain: pluginapi.ClaudePhysicalProofDomain, Coverage: "known", FromCut: &pluginapi.UsagePhysicalCut{Sequence: 10, At: hookEpoch}, ThroughCut: &pluginapi.UsagePhysicalCut{Sequence: r.Sequence, At: h.now()}}
		if r.Sequence == 10 {
			r.Physical.Coverage = "unknown"
			r.Physical.Reason = "first_quiescent_cut"
			r.Physical.ThroughCut = nil
		}
		return json.Marshal(r)
	}
	p.refreshSourceCoverage()
	return p, h, p.creds["auth-000"]
}
func physicalHookUsage(c credential, event string) map[string]any {
	u := hookUsage(c, "physical-logical", event, hookEpoch.Add(time.Second))
	e := u["SourceEvidence"].(pluginapi.UsageSourceEvidence)
	e.Physical = &pluginapi.UsagePhysicalDispatch{Version: 1, AdmissionSequence: 11, DispatchSequence: 12, StartedAt: e.UpstreamStartedAt.Add(time.Nanosecond)}
	u["SourceEvidence"] = e
	return u
}
func physicalHookSelection(t *testing.T, p *Plugin, c credential, upper time.Time) measurement.PhysicalSelection {
	t.Helper()
	var source string
	if err := p.db.QueryRow(`SELECT source_id FROM measurement_source_consumers WHERE id=?`, p.coverage.ID()).Scan(&source); err != nil {
		t.Fatal(err)
	}
	ref := measurement.SourceReference(source, measurement.SourceScope{Provider: c.Provider, AuthID: c.ID, AuthIndex: c.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1})
	from := measurement.PhysicalCutIdentity{SourceID: source, ConsumerID: p.coverage.ID(), MembershipID: c.CoverageMembership, SourceRef: ref, ManagerID: "manager-test", SinkID: "sink-test", Sequence: 10, At: hookEpoch}
	through := from
	through.Sequence = 20
	through.At = upper
	return measurement.PhysicalSelection{From: from, Through: through}
}

func TestPhysicalNativeHandleJoinsCapturedIdentityAndLeavesReportsUnknown(t *testing.T) {
	p, h, c := physicalHookPlugin(t)
	admitCoverage(t, p, "physical-logical", c.ID)
	// Today's inventory no longer matches; delayed usage must use its capture.
	p.mu.Lock()
	changed := p.creds[c.ID]
	changed.Account = "unrelated-today"
	p.creds[c.ID] = changed
	p.mu.Unlock()
	u := physicalHookUsage(c, "native-physical")
	invoke(t, p, pluginabi.MethodUsageHandle, u)
	h.mu.Lock()
	h.sequence = 20
	h.clock.Add(int64(3 * time.Second))
	h.mu.Unlock()
	p.refreshSourceCoverage()
	s := physicalHookSelection(t, p, c, h.now())
	v, err := p.coverage.SelectPhysical(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Known || len(v.Facts) != 1 || v.Facts[0].AccountID != c.Account || v.Facts[0].MembershipID != c.CoverageMembership {
		t.Fatal("native immutable join failed", v)
	}
	r := hookRecord(t, p, "native-physical")
	if r.Native.BillingMappingVerified || *r.Usage.CacheCreate5m != 25 || *r.Usage.CacheCreate1h != 75 {
		t.Fatal("physical handling changed native accounting", r)
	}
	snapshot, err := p.coverage.Snapshot(context.Background(), c.CoverageMembership, c.Provider, c.Account, hookEpoch, h.now())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Known || snapshot.PhysicalCarryInProven {
		t.Fatal("native point/time coverage promoted", snapshot)
	}
	q, err := p.coverage.BeforeMeasure(context.Background(), measurement.Query{Provider: c.Provider, AccountID: c.Account, Scope: "all", BookID: measurement.DefaultPriceBook().ID, Since: hookEpoch, AsOf: h.now()}, map[string]string{c.Account: c.CoverageMembership})
	if err != nil {
		t.Fatal(err)
	}
	report, err := p.measurement.Measure(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if report.Coverage.Observed || report.FiveHour.Known || report.SevenDay.Known || report.WarmBurn.Known || p.routingEligible {
		t.Fatal("local causal selection enabled numeric capacity or routing", report)
	}
	if hookCount(t, p, `SELECT count(*) FROM measurement_delivery_runs`) != 0 {
		t.Fatal("local proof wrote global delivery")
	}
}

func TestPhysicalNativeHandlePersistenceRefusalAndOptionalMalformedRetention(t *testing.T) {
	for _, mode := range []string{"sql", "malformed", "unsupported", "missing"} {
		t.Run(mode, func(t *testing.T) {
			p, _, c := physicalHookPlugin(t)
			admitCoverage(t, p, "physical-logical", c.ID)
			u := physicalHookUsage(c, "negative-"+mode)
			var restore func()
			if mode == "sql" {
				restore = rejectSQLInsert(t, p.db, "measurement_physical_records")
			}
			if mode == "unsupported" {
				e := u["SourceEvidence"].(pluginapi.UsageSourceEvidence)
				e.Physical.Version = 2
				u["SourceEvidence"] = e
			}
			if mode == "missing" {
				e := u["SourceEvidence"].(pluginapi.UsageSourceEvidence)
				e.Physical = nil
				u["SourceEvidence"] = e
			}
			raw := hookRaw(t, u)
			if mode == "malformed" {
				var body map[string]json.RawMessage
				_ = json.Unmarshal(raw, &body)
				var e map[string]json.RawMessage
				_ = json.Unmarshal(body["SourceEvidence"], &e)
				e["physical"] = []byte(`"invalid optional data"`)
				body["SourceEvidence"], _ = json.Marshal(e)
				raw, _ = json.Marshal(body)
			}
			_, err := p.Handle(pluginabi.MethodUsageHandle, raw)
			if restore != nil {
				restore()
			}
			if (mode == "sql" || mode == "malformed") != (err != nil) {
				t.Fatal("native acknowledgment mismatch", mode, err)
			}
			// Existing second SDK envelope decode refuses a malformed typed field,
			// after primary persistence. No plugin.go change is needed to retain it.
			r := hookRecord(t, p, "negative-"+mode)
			if r.AttemptID != "negative-"+mode || *r.Usage.CacheCreate1h != 75 {
				t.Fatal("valid accounting was dropped", r)
			}
		})
	}
}
