package historyreports

// Env-gated timing probe on the real prepared artifact. Prints only timings,
// byte sizes and counts; never names, IDs, hashes or values.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

func TestRealSpeedProbe(t *testing.T) {
	artifact := os.Getenv("PACER_REAL_ARTIFACT")
	if artifact == "" {
		t.Skip("PACER_REAL_ARTIFACT required")
	}
	ctx := context.Background()
	r, e := Open(ctx, artifact)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if p := os.Getenv("PACER_PROBE_CPU"); p != "" {
		f, e := os.Create(p)
		if e != nil {
			t.Fatal(e)
		}
		if e = pprof.StartCPUProfile(f); e != nil {
			t.Fatal(e)
		}
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	timeIt := func(name string, fn func() (any, error)) any {
		start := time.Now()
		v, e := fn()
		d := time.Since(start)
		if e != nil {
			t.Logf("%-34s err=%v %.2fs", name, e, d.Seconds())
			return nil
		}
		b, _ := json.Marshal(v)
		t.Logf("%-34s %.3fs %d bytes", name, d.Seconds(), len(b))
		return v
	}
	only := os.Getenv("PACER_PROBE_ONLY")
	want := func(s string) bool { return only == "" || strings.Contains(only, s) }
	var ws Workspace
	if want("ws") {
		v := timeIt("workspace default", func() (any, error) { return r.Report(ctx, Query{}) })
		if v != nil {
			ws = v.(Workspace)
			windows := 0
			for _, p := range ws.Profiles {
				windows += len(p.Windows)
			}
			t.Logf("profiles=%d keys=%d accounts=%d windows(all)=%d windows(profiles)=%d", len(ws.Profiles), len(ws.APIKeys), len(ws.Credentials), len(ws.Windows), windows)
		}
		timeIt("workspace default again", func() (any, error) { return r.Report(ctx, Query{}) })
		timeIt("workspace all", func() (any, error) { return r.Report(ctx, Query{ProfileReference: "all"}) })
		ps, _ := r.Profiles(ctx, -1)
		for i, p := range ps.Profiles {
			if i >= 2 {
				break
			}
			ref := p.Reference
			timeIt("workspace profile", func() (any, error) { return r.Report(ctx, Query{ProfileReference: ref}) })
		}
		for i, k := range ws.APIKeys {
			if i >= 2 {
				break
			}
			hashes := strings.Split(strings.TrimPrefix(k.ID, "keys:"), ",")
			timeIt("workspace key", func() (any, error) { return r.Report(ctx, Query{KeyHashes: hashes}) })
		}
		h := r.meta.Boundary.HMS
		day := int64(86400000)
		from := (h/day - 7) * day
		timeIt("workspace last 7 days", func() (any, error) { return r.Report(ctx, Query{From: from}) })
	}
	if want("compare") {
		timeIt("compare first (loads facts)", func() (any, error) { return r.Compare(ctx, CompareQuery{}) })
		v := timeIt("compare default", func() (any, error) { return r.Compare(ctx, CompareQuery{}) })
		if v != nil {
			view := v.(CompareView)
			for i, k := range view.APIKeys {
				if i >= 3 {
					break
				}
				id := k.ID
				timeIt("compare key", func() (any, error) { return r.Compare(ctx, CompareQuery{Key: id}) })
			}
			day := time.UnixMilli(r.meta.Boundary.HMS).UTC()
			from := day.AddDate(0, 0, -14).Format("2006-01-02")
			through := day.Format("2006-01-02")
			timeIt("compare last 14 days", func() (any, error) { return r.Compare(ctx, CompareQuery{From: from, Through: through}) })
			for _, p := range view.Profiles {
				if len(p.Pools) == 0 {
					continue
				}
				q := CompareWindowsQuery{Profile: p.Reference, Kind: p.Pools[0].Kind, Scope: p.Pools[0].Scope}
				timeIt("compare windows one pool", func() (any, error) { return r.CompareWindows(ctx, q) })
				if p.Pools[0].Latest != nil {
					ref, id := p.Reference, p.Pools[0].Latest.ID
					timeIt("evidence latest window", func() (any, error) { return r.Evidence(ctx, Query{ProfileReference: ref}, id) })
				}
				break
			}
		}
		timeIt("report default (facts)", func() (any, error) { return r.Report(ctx, Query{}) })
		ps, _ := r.Profiles(ctx, -1)
		if len(ps.Profiles) > 0 {
			ref := ps.Profiles[0].Reference
			timeIt("report profile (facts)", func() (any, error) { return r.Report(ctx, Query{ProfileReference: ref}) })
		}
	}
	if want("legacy") {
		timeIt("legacy profiles default", func() (any, error) { return r.ProfileReport(ctx, LegacyQuery{}) })
		ps, _ := r.Profiles(ctx, -1)
		if len(ps.Profiles) > 0 {
			ref := ps.Profiles[0].Reference
			timeIt("legacy profiles one", func() (any, error) {
				return r.ProfileReport(ctx, LegacyQuery{Selection: Query{ProfileReference: ref}})
			})
		}
	}
}

// Env-gated: facts built from the real artifact reproduce the original
// workspace byte for byte. Prints counts and timings only.
func TestRealQuotaFactsParity(t *testing.T) {
	artifact := os.Getenv("PACER_REAL_ARTIFACT")
	if artifact == "" {
		t.Skip("PACER_REAL_ARTIFACT required")
	}
	ctx := context.Background()
	r, e := Open(ctx, artifact)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	start := time.Now()
	q, e := r.service.BuildQuotaFacts(ctx, r.meta.Pin)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(q)
	t.Logf("facts build %.2fs, %d bytes, %d buckets, %d windows, order_sensitive=%v", time.Since(start).Seconds(), len(raw), len(q.Buckets), len(q.Windows), q.OrderSensitive)
	start = time.Now()
	var q2 cpamp.QuotaFacts
	if e = json.Unmarshal(raw, &q2); e != nil {
		t.Fatal(e)
	}
	t.Logf("facts decode %.3fs", time.Since(start).Seconds())
	filters := quotaFactFilters(t, r, r.meta.Pin)
	limit := len(filters)
	if os.Getenv("PACER_REAL_FILTER_LIMIT") != "" {
		fmtLimit := 0
		for _, c := range os.Getenv("PACER_REAL_FILTER_LIMIT") {
			fmtLimit = fmtLimit*10 + int(c-'0')
		}
		limit = min(limit, fmtLimit)
	}
	if os.Getenv("PACER_REAL_PROGRESS") != "" {
		fmt.Fprintf(os.Stderr, "filters %d, limit %d\n", len(filters), limit)
	}
	var slow, fast time.Duration
	n := 0
	for i := 0; i < limit; i++ {
		f := filters[i]
		s0 := time.Now()
		want, we := r.service.WorkspaceOriginal(ctx, f)
		slow += time.Since(s0)
		f0 := time.Now()
		got, ok, ge := r.service.FactsWorkspace(ctx, &q2, f, cpamp.FactsOptions{})
		fast += time.Since(f0)
		if !ok || (we == nil) != (ge == nil) {
			t.Fatalf("case %d: ok=%v errors %v / %v", i, ok, we, ge)
		}
		if we != nil {
			continue
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if !bytes.Equal(a, b) {
			at := 0
			for at < len(a) && at < len(b) && a[at] == b[at] {
				at++
			}
			t.Fatalf("case %d differs at byte %d of %d/%d", i, at, len(a), len(b))
		}
		n++
		if os.Getenv("PACER_REAL_PROGRESS") != "" && n%25 == 0 {
			fmt.Fprintf(os.Stderr, "progress %d/%d original %.0fs\n", n, limit, slow.Seconds())
		}
	}
	t.Logf("%d of %d filters byte-identical; original %.1fs total (%.2fs avg), facts %.3fs total (%.4fs avg)", n, len(filters), slow.Seconds(), slow.Seconds()/float64(max(limit, 1)), fast.Seconds(), fast.Seconds()/float64(max(limit, 1)))
}
