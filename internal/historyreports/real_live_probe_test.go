package historyreports

// Env-gated timing probe for the continuous record on the real prepared
// artifact. Pacer's own week after H is simulated by replaying every account's
// last imported week shifted by seven days (requests and readings), so the
// volume is real. Prints only timings and counts; never names, IDs or values.

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func TestRealLiveProbe(t *testing.T) {
	artifact := os.Getenv("PACER_REAL_ARTIFACT")
	if artifact == "" {
		t.Skip("PACER_REAL_ARTIFACT required")
	}
	ctx := context.Background()
	r, err := Open(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := frozen.Filter.At
	shift := 7 * 24 * time.Hour
	accounts, err := r.service.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var events []ForwardEvent
	var readings []NativeReading
	origins := ForwardOrigins{}
	for _, a := range accounts {
		tail, err := r.tail(ctx, frozen, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		native := "native-" + strconv.Itoa(len(origins.Certificates))
		origins.Certificates = append(origins.Certificates, ForwardOriginCertificate{LegacyAccount: a.ID, Provider: a.Provider, SourceID: liveSource, NativeAccount: native, NativeCredentialRef: "cred", StartMS: h})
		for _, e := range tail.events {
			if e.Started < h-shift.Milliseconds() {
				continue
			}
			ev := e
			ev.ID, ev.Value, ev.Account = 0, nil, native
			ev.Started += shift.Milliseconds()
			ev.Completed += shift.Milliseconds()
			ev.Recorded += shift.Milliseconds()
			events = append(events, ForwardEvent{ID: "probe-" + strconv.Itoa(len(events)), SourceID: liveSource, StartedAt: time.UnixMilli(ev.Started), Attribution: measurement.Attribution{CredentialRef: "cred"}, Event: ev})
		}
		for _, o := range tail.obs {
			if o.Observed < h-shift.Milliseconds() || o.Used == nil || o.Duration <= 0 {
				continue
			}
			remaining := 100 - *o.Used
			source := "api"
			if o.Source == "response_header" {
				source = "header"
			}
			readings = append(readings, NativeReading{ID: int64(len(readings) + 1), Account: native, Provider: a.Provider, Scope: o.Scope, Seconds: o.Duration / 1000, Remaining: &remaining, Reset: time.UnixMilli(o.Reset + shift.Milliseconds()), At: time.UnixMilli(o.Observed + shift.Milliseconds()), Source: source})
		}
	}
	t.Logf("tails read %.2fs: %d simulated requests, %d readings, %d accounts", time.Since(start).Seconds(), len(events), len(readings), len(accounts))
	in := LiveInput{Now: time.UnixMilli(h).Add(shift), Native: UsageNative{Events: events, Origins: origins}, Readings: readings, Source: liveSource}
	for i := 0; i < 3; i++ {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		live, stats, err := r.BuildLive(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		r.UseLive(live)
		started := time.Now()
		view, err := r.Compare(ctx, CompareQuery{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("build %d: %d ms (accounts %d, live windows %d, ambiguous %d, skipped readings %d), alloc %.0f MB, compare %.1f ms, profiles %d", i, stats.ElapsedMS, stats.Accounts, stats.Windows, stats.Ambiguous, stats.ReadingsSkipped, float64(after.TotalAlloc-before.TotalAlloc)/1e6, float64(time.Since(started).Microseconds())/1000, len(view.Profiles))
	}
	// Inspect and Windows on the live record: every pool's latest window and
	// average; per-model figures must add up to the totals.
	view, _ := r.Compare(ctx, CompareQuery{}, nil)
	inspected, slowest := 0, time.Duration(0)
	for _, p := range view.Profiles {
		for _, pool := range p.Pools {
			queries := []InspectQuery{{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope}}
			if pool.Latest != nil {
				queries = append(queries, InspectQuery{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope, Window: pool.Latest.ID})
			}
			for _, q := range queries {
				started := time.Now()
				v, err := r.Inspect(ctx, q, nil)
				if err != nil {
					t.Fatal(err)
				}
				slowest = max(slowest, time.Since(started))
				requests, share := int64(0), 0.0
				for _, m := range v.Models {
					requests += m.Requests
					share += m.SharePercent
				}
				// Shares add up to 100%, or are all 0 when nothing has value or tokens.
				if requests != v.Totals.Requests || share != 0 && (share < 99.999 || share > 100.001) {
					t.Fatalf("models do not add up: %d/%d requests, %.4f%%", requests, v.Totals.Requests, share)
				}
				inspected++
			}
			if _, err := r.CompareWindows(ctx, CompareWindowsQuery{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("inspect: %d inspections, slowest %.1f ms; every model breakdown adds up", inspected, float64(slowest.Microseconds())/1000)
}
