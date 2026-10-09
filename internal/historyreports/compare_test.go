package historyreports

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

func compareOriginal(t *testing.T, r *Reader, apiKey string, from, to int64) Workspace {
	t.Helper()
	f := r.meta.Pin
	f.APIKey, f.From, f.To = apiKey, from, to
	w, e := r.service.WorkspaceOriginal(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	return w
}

func TestCompareMatchesOriginalWorkspace(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		reader func(*testing.T) *Reader
	}{{"workspace-fixture", fixtureReader}, {"historical-fixture", historicalFixture}} {
		t.Run(c.name, func(t *testing.T) {
			r := c.reader(t)
			unfiltered := compareOriginal(t, r, "", 0, 0)
			view, e := r.Compare(ctx, CompareQuery{}, nil)
			if e != nil {
				t.Fatal(e)
			}
			if len(view.Profiles) != len(unfiltered.Profiles) || view.Version != CompareVersion || view.KeyFiltered {
				t.Fatalf("profiles %d/%d", len(view.Profiles), len(unfiltered.Profiles))
			}
			measured := 0
			for i, p := range unfiltered.Profiles {
				got := view.Profiles[i]
				if got.Reference != "profile:"+hash(p.ID) || got.Name != p.Name || !reflect.DeepEqual(got.Lifetime, compareTotals(p.Lifetime)) || !reflect.DeepEqual(got.Selected, compareTotals(p.Selected)) {
					t.Fatalf("profile %d header/totals differ", i)
				}
				if len(got.Current) != len(p.Current) {
					t.Fatalf("profile %d current meters %d/%d", i, len(got.Current), len(p.Current))
				}
				for _, pool := range got.Pools {
					// Latest = the original Compare's default pick in this pool.
					var want *Window
					count := 0
					for j := range p.Windows {
						w := p.Windows[j]
						if w.Kind != pool.Kind || w.Scope != pool.Scope {
							continue
						}
						count++
						if want == nil && w.Normalization != nil && w.Normalization.Status == "available" {
							want = &p.Windows[j]
						}
					}
					if count != pool.Windows || pool.Latest == nil {
						t.Fatalf("profile %d pool %s/%s windows %d/%d", i, pool.Kind, pool.Scope, pool.Windows, count)
					}
					if want != nil {
						measured++
						got := *pool.Latest
						if got.Models == nil || got.Excluded {
							t.Fatalf("profile %d pool %s/%s latest lacks its model mix", i, pool.Kind, pool.Scope)
						}
						got.Models = nil
						if !reflect.DeepEqual(got, compareWindow(*want)) {
							t.Fatalf("profile %d pool %s/%s latest differs", i, pool.Kind, pool.Scope)
						}
					}
					found := false
					for _, g := range p.Rates {
						if g.Kind == pool.Kind && g.Scope == pool.Scope {
							n := g.Normalization
							found = reflect.DeepEqual(pool.Average, compareRate(&n))
						}
					}
					if !found {
						t.Fatalf("profile %d pool %s/%s average differs", i, pool.Kind, pool.Scope)
					}
				}
			}
			if measured == 0 {
				t.Fatal("fixture has no measured window; comparison is vacuous")
			}

			// A key keeps account-wide rates and filters recorded totals exactly.
			for _, k := range view.APIKeys {
				keyed, e := r.Compare(ctx, CompareQuery{Key: k.ID}, nil)
				if e != nil {
					t.Fatal(e)
				}
				original := compareOriginal(t, r, k.ID, 0, 0)
				for i, p := range original.Profiles {
					if !reflect.DeepEqual(keyed.Profiles[i].Selected, compareTotals(p.Selected)) || !reflect.DeepEqual(keyed.Profiles[i].Lifetime, compareTotals(p.Lifetime)) {
						t.Fatalf("key %s profile %d totals differ", k.ID, i)
					}
					for j, pool := range keyed.Profiles[i].Pools {
						base := view.Profiles[i].Pools[j]
						if !reflect.DeepEqual(pool.Average, base.Average) || pool.Latest.ID != base.Latest.ID || !reflect.DeepEqual(pool.Latest.Rate, base.Latest.Rate) {
							t.Fatalf("key %s profile %d: rates must stay account-wide", k.ID, i)
						}
						for _, w := range p.Windows {
							if w.ID == pool.Latest.ID && !reflect.DeepEqual(pool.Latest.Selected, compareTotals(w.Selected)) {
								t.Fatalf("key %s profile %d: window selection differs", k.ID, i)
							}
						}
					}
				}
			}

			// Whole-day bounds select requests exactly and windows by overlap.
			day := r.meta.Boundary.HMS / 86400000 * 86400000
			for _, span := range []int64{1, 3, 10} {
				from := time.UnixMilli(day - span*86400000).UTC().Format("2006-01-02")
				through := time.UnixMilli(day - 86400000).UTC().Format("2006-01-02")
				dated, e := r.Compare(ctx, CompareQuery{From: from, Through: through}, nil)
				if e != nil {
					t.Fatal(e)
				}
				original := compareOriginal(t, r, "", day-span*86400000, day)
				for i, p := range original.Profiles {
					if !reflect.DeepEqual(dated.Profiles[i].Selected, compareTotals(p.Selected)) {
						t.Fatalf("span %d profile %d: dated totals differ", span, i)
					}
					total := 0
					for _, pool := range dated.Profiles[i].Pools {
						total += pool.Windows
					}
					if total != len(p.Windows) {
						t.Fatalf("span %d profile %d: windows %d/%d", span, i, total, len(p.Windows))
					}
				}
			}
		})
	}
}

func TestCompareInputsWindowsAndPreparation(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	for _, q := range []CompareQuery{{Key: "not-a-key"}, {From: "2026-1-01"}, {From: "2026-10-02", Through: "2026-10-01"}, {Through: "1999-12-31"}} {
		if _, e := r.Compare(ctx, q, nil); !errors.Is(e, ErrInput) {
			t.Fatalf("%+v accepted: %v", q, e)
		}
	}
	view, e := r.Compare(ctx, CompareQuery{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(view)
	if len(b) > 256<<10 {
		t.Fatalf("compare view is %d bytes", len(b))
	}
	listed := 0
	for _, p := range view.Profiles {
		for _, pool := range p.Pools {
			list, e := r.CompareWindows(ctx, CompareWindowsQuery{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope}, nil)
			if e != nil {
				t.Fatal(e)
			}
			if len(list.Windows) != pool.Windows {
				t.Fatalf("list %d/%d", len(list.Windows), pool.Windows)
			}
			for i := 1; i < len(list.Windows); i++ {
				if list.Windows[i].ResetMS > list.Windows[i-1].ResetMS {
					t.Fatal("windows not newest first")
				}
			}
			listed += len(list.Windows)
		}
	}
	if listed == 0 {
		t.Fatal("no windows listed")
	}
	if _, e = r.CompareWindows(ctx, CompareWindowsQuery{Profile: "profile:" + hash("missing"), Kind: "weekly", Scope: "all"}, nil); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	if _, e = r.CompareWindows(ctx, CompareWindowsQuery{Profile: view.Profiles[0].Reference, Kind: "monthly", Scope: "all"}, nil); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	// Compare never needs the database once facts are loaded.
	if e = r.db.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Compare(ctx, CompareQuery{Key: cpamp.UnknownAPIKey}, nil); e != nil {
		t.Fatalf("compare touched the database: %v", e)
	}
	// Artifacts prepared before quota facts report the missing preparation.
	old := historicalFixture(t)
	old.meta.QuotaFacts = false
	if _, e = old.Compare(ctx, CompareQuery{}, nil); !errors.Is(e, ErrNotPrepared) {
		t.Fatal(e)
	}
}
