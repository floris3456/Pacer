package pacer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestUsageLiveFollowsPacersOwnRecord(t *testing.T) {
	ctx := context.Background()
	f := preparedRuntime(t)
	p := f.open(t, nil)
	first := forwardWait(t, p, func(v ForwardRuntimeStatus) bool { return v.State == "ready" && !v.Active })
	reader := reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	p.mu.Lock()
	p.reports = reader
	p.mu.Unlock()
	h := time.UnixMilli(reader.ImportedThrough(ctx)).UTC()
	if !hookEpoch.After(h) {
		t.Fatal("fixture: Pacer's requests must start after H")
	}
	// Nothing recorded since H yet: the page shows the imported record.
	if err := p.refreshUsageLive(ctx); err != nil || p.usageLiveStatus().State != "waiting" {
		t.Fatal(err, p.usageLiveStatus())
	}
	c := p.creds[f.c.ID]
	admitCoverage(t, p, "live-logical", c.ID)
	f.h.clock.Add(int64(3 * time.Second))
	invoke(t, p, pluginabi.MethodUsageHandle, forwardMixed(c, "live-logical", "live-physical", hookEpoch.Add(time.Second)))
	head, _ := p.measurement.ReportFeedHead(ctx)
	forwardWait(t, p, func(v ForwardRuntimeStatus) bool {
		return v.State == "ready" && !v.Active && v.Projected == head.Through && v.Latest != first.Latest
	})
	reset := hookEpoch.Add(3 * time.Hour)
	for i, remaining := range []float64{80, 70} {
		at := hookEpoch.Add(time.Duration(2+i) * time.Second)
		v := remaining
		w := observation.Window{Provider: c.Provider, Scope: "all", Seconds: 5 * 3600, Remaining: &v, Reset: reset,
			Usage: observation.Field{At: at, Source: observation.HeaderSource, Quality: observation.Measured}, ResetEvidence: observation.Field{At: at, Source: observation.HeaderSource, Quality: observation.Measured}}
		if err := p.observer.Observe(c.Account, c.Provider, []observation.Window{w}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.refreshUsageLive(ctx); err != nil {
		t.Fatal(err)
	}
	status := p.usageLiveStatus()
	if status.State != "live" || status.Stats.Requests != 1 || status.Stats.Readings != 2 || status.Stats.NewAccounts != 1 || status.ImportedThroughMS != h.UnixMilli() {
		t.Fatalf("%+v", status)
	}
	got := usageCall(p, "POST", "profiles", `{}`)
	var view historyreports.CompareView
	if got.StatusCode != 200 || json.Unmarshal(got.Body, &view) != nil || view.Live == nil || view.Live.State != "live" || view.FrozenAtMS != p.now().UnixMilli() || view.ImportedThroughMS != h.UnixMilli() {
		t.Fatal(got.StatusCode, string(got.Body))
	}
	// No new evidence and no transition: no rebuild. Later, staleness moves on.
	built := status.BuiltAtMS
	f.h.clock.Add(int64(time.Minute))
	if err := p.refreshUsageLive(ctx); err != nil || p.usageLiveStatus().BuiltAtMS != built {
		t.Fatal("rebuilt without a reason", err)
	}
	f.h.clock.Add(int64(usageLiveRefresh))
	if err := p.refreshUsageLive(ctx); err != nil || p.usageLiveStatus().BuiltAtMS == built {
		t.Fatal("never refreshed", err)
	}
	// A new artifact starts over; no artifact switches the record off.
	other := reportFixtureFrom(t, "historical-golden/historical-fixture.sql.gz", 240, 65)
	p.mu.Lock()
	p.reports = other
	p.mu.Unlock()
	if err := p.refreshUsageLive(ctx); err != nil || p.usageLiveStatus().State != "live" || p.usageLiveStatus().Stats.Requests != 1 {
		t.Fatal("new artifact", err, p.usageLiveStatus())
	}
	p.mu.Lock()
	p.reports = nil
	p.mu.Unlock()
	if err := p.refreshUsageLive(ctx); err != nil || p.usageLiveStatus().State != "off" {
		t.Fatal("no artifact", err)
	}
	p.mu.Lock()
	p.reports = other
	p.mu.Unlock()
}
