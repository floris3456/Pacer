package historyreports

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

func TestWindowExclusionsAreRevisionedAndNeverDeleted(t *testing.T) {
	ctx := context.Background()
	s, _ := forwardLedger(t)
	profile, window := "profile:"+hash("p"), hash("w")[:32]
	empty, err := s.WindowExclusions(ctx)
	if err != nil || empty.Revision != 0 || len(empty.Excluded) != 0 {
		t.Fatal(empty, err)
	}
	at := time.UnixMilli(1790812800000)
	one, err := s.SetWindowExclusion(ctx, 0, profile, window, true, "ran a benchmark", at)
	if err != nil || one.Revision != 1 || len(one.Excluded) != 1 || one.Excluded[0].Note != "ran a benchmark" || one.Excluded[0].AtMS != at.UnixMilli() {
		t.Fatal(one, err)
	}
	if _, err = s.SetWindowExclusion(ctx, 0, profile, window, false, "", at); !errors.Is(err, ErrForwardConflict) {
		t.Fatal("stale revision accepted", err)
	}
	for _, bad := range []struct {
		profile, window, note string
		excluded              bool
	}{{"p", window, "", true}, {profile, "not a window", "", true}, {profile, window, "sk-secret", true}, {profile, window, "line\nbreak", true}, {profile, window, "note without exclusion", false}} {
		if _, err = s.SetWindowExclusion(ctx, 1, bad.profile, bad.window, bad.excluded, bad.note, at); !errors.Is(err, ErrInput) {
			t.Fatal("invalid exclusion accepted", bad, err)
		}
	}
	two, err := s.SetWindowExclusion(ctx, 1, profile, window, false, "", at)
	if err != nil || two.Revision != 2 || len(two.Excluded) != 0 {
		t.Fatal(two, err)
	}
	// Every revision stays readable: nothing is overwritten or deleted.
	var older WindowExclusions
	if err = s.objectNamed(ctx, exclusionKind, exclusionName(1), &older); err != nil || len(older.Excluded) != 1 {
		t.Fatal("revision 1 lost", older, err)
	}
	latest, err := s.WindowExclusions(ctx)
	if err != nil || latest.Revision != 2 || len(latest.Map()[profile]) != 0 {
		t.Fatal(latest, err)
	}
}

// A measured window excluded from a profile's average leaves the window listed
// but recomputes the average without it; nothing else changes.
func TestCompareAverageHonorsExclusions(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	view, err := r.Compare(ctx, CompareQuery{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, p := range view.Profiles {
		for _, pool := range p.Pools {
			if pool.Average.Status != "available" || pool.Average.Samples < 1 || pool.Latest == nil || pool.Latest.Rate.Status != "available" {
				continue
			}
			ex := Exclusions{p.Reference: {pool.Latest.ID: "unfair"}}
			after, err := r.Compare(ctx, CompareQuery{}, ex)
			if err != nil {
				t.Fatal(err)
			}
			var got *ComparePool
			for i := range after.Profiles {
				if after.Profiles[i].Reference == p.Reference {
					for j := range after.Profiles[i].Pools {
						if after.Profiles[i].Pools[j].Kind == pool.Kind && after.Profiles[i].Pools[j].Scope == pool.Scope {
							got = &after.Profiles[i].Pools[j]
						}
					}
				}
			}
			if got == nil || got.Windows != pool.Windows || got.Excluded != 1 || got.Latest == nil || !got.Latest.Excluded || got.Latest.Note != "unfair" || got.Latest.ID != pool.Latest.ID {
				t.Fatalf("excluded window must stay listed and marked: %+v", got)
			}
			if pool.Average.Samples == 1 {
				if got.Average.Status != "unavailable" {
					t.Fatal("the only measured window was excluded but the average stayed available")
				}
			} else if got.Average.Samples != pool.Average.Samples-1 || got.Average.DeltaPP >= pool.Average.DeltaPP {
				t.Fatalf("average still counts the excluded window: %+v -> %+v", pool.Average, got.Average)
			}
			list, err := r.CompareWindows(ctx, CompareWindowsQuery{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope}, ex)
			if err != nil || len(list.Windows) != pool.Windows {
				t.Fatal(list, err)
			}
			marked := 0
			for _, w := range list.Windows {
				if w.Excluded {
					marked++
				}
			}
			if marked != 1 {
				t.Fatal("window list must mark exactly the excluded window", marked)
			}
			// Another profile's exclusion does not touch this profile.
			other, _ := r.Compare(ctx, CompareQuery{}, Exclusions{"profile:" + hash("someone-else"): {pool.Latest.ID: ""}})
			for _, q := range other.Profiles {
				if q.Reference == p.Reference {
					for _, op := range q.Pools {
						if op.Kind == pool.Kind && op.Scope == pool.Scope && op.Average.DeltaPP != pool.Average.DeltaPP {
							t.Fatal("exclusion leaked across profiles")
						}
					}
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("fixture has no measured average to exclude from")
	}
}

func TestInspectWindowAndAverage(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	view, err := r.Compare(ctx, CompareQuery{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	inspected := 0
	for _, p := range view.Profiles {
		for _, pool := range p.Pools {
			if pool.Latest == nil {
				continue
			}
			win, err := r.Inspect(ctx, InspectQuery{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope, Window: pool.Latest.ID}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if win.Mode != "window" || win.Window == nil || win.Window.ID != pool.Latest.ID || win.Windows != 1 || win.ProfileName != p.Name {
				t.Fatal("wrong window inspected", win.Mode)
			}
			sum, requests := 0.0, int64(0)
			var tokens InspectTokens
			for _, m := range win.Models {
				sum += m.SharePercent
				requests += m.Requests
				tokens.Input += m.Tokens.Input
				tokens.CacheWrite += m.Tokens.CacheWrite
				tokens.CacheRead += m.Tokens.CacheRead
				tokens.Output += m.Tokens.Output
				tokens.Total += m.Tokens.Total
			}
			if len(win.Models) > 0 && !near(sum, 100) || requests != win.Totals.Requests || tokens != win.Totals.Tokens {
				t.Fatalf("models do not add up to the window totals: share %.6f requests %d/%d", sum, requests, win.Totals.Requests)
			}
			if win.Totals.Requests != pool.Latest.Selected.Requests || win.Window.Selected != pool.Latest.Selected || win.Totals.ValueLowNanoUSD != pool.Latest.Selected.ValueLowNanoUSD {
				t.Fatal("inspect totals disagree with the card's recorded requests")
			}
			if win.Rate.Status == "available" && win.PerPercent == nil {
				t.Fatal("a measured window must say what 1% buys")
			}
			if len(win.Keys) > 0 {
				keySum := 0.0
				for _, k := range win.Keys {
					keySum += k.SharePercent
				}
				if !near(keySum, 100) {
					t.Fatal("key shares must add up to 100%", keySum)
				}
			}
			avg, err := r.Inspect(ctx, InspectQuery{Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope}, nil)
			if err != nil || avg.Mode != "average" || avg.Window != nil {
				t.Fatal(avg.Mode, err)
			}
			if avg.Rate.Status != pool.Average.Status || avg.Rate.Samples != pool.Average.Samples {
				t.Fatal("average inspect must use the card's average")
			}
			// A key filter keeps the account-wide rate and drops the key list.
			keyed, err := r.Inspect(ctx, InspectQuery{CompareQuery: CompareQuery{Key: cpamp.UnknownAPIKey}, Profile: p.Reference, Kind: pool.Kind, Scope: pool.Scope, Window: pool.Latest.ID}, nil)
			if err != nil || len(keyed.Keys) != 0 || keyed.Rate.Status != win.Rate.Status || keyed.Totals.Requests > win.Totals.Requests {
				t.Fatal("key-filtered inspect", err)
			}
			inspected++
		}
	}
	if inspected == 0 {
		t.Fatal("nothing inspected")
	}
	ref := view.Profiles[0].Reference
	for _, q := range []InspectQuery{
		{Profile: ref, Kind: "monthly", Scope: "all"},
		{Profile: ref, Kind: "weekly"},
		{Profile: "profile:" + hash("missing"), Kind: "weekly", Scope: "all"},
		{Profile: ref, Kind: "weekly", Scope: "all", Window: hash("no-such-window")[:32]},
		{CompareQuery: CompareQuery{Key: "sk-raw"}, Profile: ref, Kind: "weekly", Scope: "all"},
	} {
		if _, err = r.Inspect(ctx, q, nil); !errors.Is(err, ErrInput) {
			t.Fatal("invalid inspect accepted", q, err)
		}
	}
}

func TestModelSharesFoldIntoOther(t *testing.T) {
	models := map[string]cpamp.ProfileTotals{}
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		models[name] = cpamp.ProfileTotals{Valued: true, Value: cpamp.APIValue{Low: int64(10 * (5 - i)), High: int64(10 * (5 - i))}}
	}
	got := modelShares(models, 3)
	if len(got) != 4 || got[0].Model != "a" || got[3].Model != "Other" {
		t.Fatal(got)
	}
	total := 0.0
	for _, m := range got {
		total += m.SharePercent
	}
	if math.Abs(total-100) > 1e-9 {
		t.Fatal(total)
	}
	// Without values the mix falls back to tokens.
	if got = modelShares(map[string]cpamp.ProfileTotals{"x": {Tokens: cpamp.Tokens{Total: 3}}, "y": {Tokens: cpamp.Tokens{Total: 1}}}, 3); len(got) != 2 || got[0].SharePercent != 75 {
		t.Fatal(got)
	}
}
