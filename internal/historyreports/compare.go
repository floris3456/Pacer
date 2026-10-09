package historyreports

// Compare is the instant Usage Quota view over the frozen history: every profile
// on one page, its newest measured 7-day and 5-hour windows and the measured
// history average. It reads only the prepared quota facts (no SQLite, no
// re-pricing), so any key or date selection answers in milliseconds.
//
// Semantics that differ from the original workspace, by design:
//   - Dates are whole UTC days. A window that overlaps the dates counts whole:
//     its rate uses its complete measured interval, not one clipped to the dates.
//   - Rates ($ per 1%) describe the account's shared quota meter, so they stay
//     account-wide under a key filter. The key changes only the recorded
//     requests/value and the key's share of each window.

import (
	"context"
	"regexp"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

const CompareVersion = "pacer-quota-compare-v1"

type CompareQuery struct {
	// Key is "", "unattributed", an id from api_keys, or one 64-hex key hash.
	Key string `json:"key,omitempty"`
	// From and Through are inclusive UTC days, YYYY-MM-DD.
	From    string `json:"from,omitempty"`
	Through string `json:"through,omitempty"`
}

type CompareWindowsQuery struct {
	CompareQuery
	Profile string `json:"profile"` // profile reference
	Kind    string `json:"kind"`    // weekly or 5h
	Scope   string `json:"scope"`
}

type CompareTotals struct {
	Requests            int64  `json:"requests"`
	Failed              int64  `json:"failed"`
	Tokens              int64  `json:"tokens"`
	Valued              bool   `json:"valued"`
	ValueLowNanoUSD     int64  `json:"value_low_nano_usd"`
	ValueHighNanoUSD    int64  `json:"value_high_nano_usd"`
	ValueBasis          string `json:"value_basis,omitempty"`
	MissingPrice        int64  `json:"missing_price_requests"`
	InvalidCounters     int64  `json:"invalid_counter_requests"`
	CapturedCostNanoUSD int64  `json:"captured_cost_nano_usd"`
	Unpriced            int64  `json:"unpriced_requests"`
}

type CompareRate struct {
	Status               string            `json:"status"`
	Reason               string            `json:"reason,omitempty"`
	USDPerPercent        *cpamp.ValueRange `json:"usd_per_percent"`
	EquivalentPerPercent *cpamp.ValueRange `json:"equivalent_tokens_per_percent"`
	RoundingUSDPerPct    *cpamp.ValueRange `json:"rounding_usd_per_percent,omitempty"`
	ReferenceModel       string            `json:"reference_model,omitempty"`
	ReferenceName        string            `json:"reference_name,omitempty"`
	DeltaPP              float64           `json:"delta_pp"`
	StartPercent         *float64          `json:"start_percent"`
	EndPercent           *float64          `json:"end_percent"`
	FromMS               int64             `json:"from_ms"`
	ThroughMS            int64             `json:"through_ms"`
	ContainedRequests    int64             `json:"contained_requests"`
	BoundaryRequests     int64             `json:"boundary_requests"`
	Samples              int               `json:"samples"`
	Skipped              int               `json:"skipped_windows"`
	PollTiming           bool              `json:"poll_timing_approximate"`
	Notes                []string          `json:"notes"`
}

type CompareWindow struct {
	ID                string        `json:"id"`
	Label             string        `json:"label"`
	Kind              string        `json:"kind"`
	Scope             string        `json:"scope"`
	StartMS           int64         `json:"start_ms"`
	ResetMS           int64         `json:"reset_ms"`
	EndMS             int64         `json:"end_ms"`
	Status            string        `json:"status"`
	UsedPercent       *float64      `json:"used_percent"`
	Rate              CompareRate   `json:"rate"`
	Selected          CompareTotals `json:"selected"`
	AccountValue      CompareTotals `json:"account_value"`
	TokenSharePercent *float64      `json:"token_share_percent"`
	// Models is the profile's model mix in this window (share of value), top 3 + other.
	Models   []ModelShare `json:"models"`
	Excluded bool         `json:"excluded"`
	Note     string       `json:"note,omitempty"`
}

type ModelShare struct {
	Model        string  `json:"model"`
	SharePercent float64 `json:"share_percent"`
}

type CompareMeter struct {
	Kind           string   `json:"kind"`
	Scope          string   `json:"scope"`
	Label          string   `json:"label"`
	UsedPercent    *float64 `json:"used_percent"`
	ResetMS        int64    `json:"reset_ms"`
	LastObservedMS int64    `json:"last_observed_ms"`
	Stale          bool     `json:"stale"`
}

type ComparePool struct {
	Kind    string `json:"kind"`
	Scope   string `json:"scope"`
	Main    bool   `json:"main"`
	Windows int    `json:"windows"`
	// Latest is the newest window with a measured rate (the original Compare's
	// default); NewestUnmeasured is set when a newer window has no rate yet.
	Latest           *CompareWindow `json:"latest"`
	NewestUnmeasured *CompareWindow `json:"newest_unmeasured,omitempty"`
	Average          CompareRate    `json:"average"`
	// AverageModels is the model mix over the windows the average used.
	AverageModels []ModelShare `json:"average_models"`
	// Excluded counts this pool's windows in range that the user excluded.
	Excluded int `json:"excluded"`
}

type CompareProfile struct {
	Reference   string         `json:"reference"`
	Name        string         `json:"name"`
	Provider    string         `json:"provider"`
	Archived    bool           `json:"archived"`
	Credentials int            `json:"credentials"`
	FirstMS     int64          `json:"first_ms"`
	LastMS      int64          `json:"last_ms"`
	Lifetime    CompareTotals  `json:"lifetime"`
	Selected    CompareTotals  `json:"selected"`
	Current     []CompareMeter `json:"current"`
	Pools       []ComparePool  `json:"pools"`
}

type ComparePriceBook struct {
	ID         string `json:"id"`
	Policy     string `json:"policy"`
	CapturedMS int64  `json:"captured_ms"`
	Historical string `json:"historical_wording,omitempty"`
}

type CompareView struct {
	Version    string `json:"version"`
	FrozenAtMS int64  `json:"frozen_at_ms"` // data as of (now when Pacer's own record continues it)
	// ImportedThroughMS is H: imported history before it, Pacer's own record after.
	ImportedThroughMS int64 `json:"imported_through_ms"`
	// Live is the continuous record's status, set by the caller.
	Live        *LiveStatus      `json:"live,omitempty"`
	Query       CompareQuery     `json:"query"`
	KeyFiltered bool             `json:"key_filtered"`
	PriceBook   ComparePriceBook `json:"price_book"`
	APIKeys     []cpamp.APIKey   `json:"api_keys"`
	Profiles    []CompareProfile `json:"profiles"`
	Notes       []string         `json:"notes"`
	// Set by the caller from the exclusion store.
	ExclusionsRevision int64 `json:"exclusions_revision"`
	ExclusionsEditable bool  `json:"exclusions_editable"`
}

type CompareWindowList struct {
	Version string          `json:"version"`
	Query   CompareQuery    `json:"query"`
	Profile string          `json:"profile"`
	Kind    string          `json:"kind"`
	Scope   string          `json:"scope"`
	Windows []CompareWindow `json:"windows"`
}

var compareDay = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func compareBounds(q CompareQuery) (int64, int64, error) {
	parse := func(s string) (int64, error) {
		if !compareDay.MatchString(s) {
			return 0, ErrInput
		}
		t, e := time.Parse("2006-01-02", s)
		if e != nil || t.Year() < 2000 || t.Year() > 2200 {
			return 0, ErrInput
		}
		return t.UnixMilli(), nil
	}
	var from, to int64
	var e error
	if q.From != "" {
		if from, e = parse(q.From); e != nil {
			return 0, 0, e
		}
	}
	if q.Through != "" {
		if to, e = parse(q.Through); e != nil {
			return 0, 0, e
		}
		to += 86400000
	}
	if to > 0 && from >= to {
		return 0, 0, ErrInput
	}
	return from, to, nil
}

func compareKey(q *cpamp.QuotaFacts, key string) error {
	if key == "" || key == cpamp.UnknownAPIKey || validHash(key) {
		return nil
	}
	for _, k := range q.APIKeys {
		if k.ID == key {
			return nil
		}
	}
	return ErrInput
}

func compareTotals(t cpamp.ProfileTotals) CompareTotals {
	return CompareTotals{Requests: t.Requests, Failed: t.Failed, Tokens: t.Tokens.Total, Valued: t.Valued, ValueLowNanoUSD: t.Value.Low, ValueHighNanoUSD: t.Value.High, ValueBasis: t.Value.Basis, MissingPrice: t.Value.Missing, InvalidCounters: t.Value.Invalid, CapturedCostNanoUSD: t.CostNano, Unpriced: t.Unpriced}
}

func compareRate(n *cpamp.ProfileNormalization) CompareRate {
	if n == nil {
		return CompareRate{Status: "unavailable", Reason: "No measurement for this window.", Notes: []string{}}
	}
	notes := append([]string{}, n.Notes...)
	return CompareRate{Status: n.Status, Reason: n.Reason, USDPerPercent: n.USDPerPercent, EquivalentPerPercent: n.EquivalentPerPercent, RoundingUSDPerPct: n.RoundingUSDPerPercent, ReferenceModel: n.Reference.Model, ReferenceName: n.Reference.Name, DeltaPP: n.Delta, StartPercent: n.StartPercent, EndPercent: n.EndPercent, FromMS: n.From, ThroughMS: n.Through, ContainedRequests: n.ContainedRequests, BoundaryRequests: n.BoundaryRequests, Samples: n.Samples, Skipped: n.Skipped, PollTiming: n.PollTiming, Notes: notes}
}

func compareWindow(w cpamp.QuotaUsageWindow) CompareWindow {
	return CompareWindow{ID: w.ID, Label: w.Label, Kind: w.Kind, Scope: w.Scope, StartMS: w.Start, ResetMS: w.Reset, EndMS: w.AttributionEnd, Status: w.Status, UsedPercent: w.Used, Rate: compareRate(w.Normalization), Selected: compareTotals(w.Selected), AccountValue: compareTotals(w.AccountUsage), TokenSharePercent: w.TokenShare}
}

func mainScope(scope string) bool {
	return scope == "all" || scope == "codex-main" || scope == "gemini"
}

func (r *Reader) compareWorkspace(ctx context.Context, q CompareQuery, ex Exclusions) (*cpamp.QuotaFacts, Workspace, error) {
	facts, e := r.usageFacts(ctx)
	if e != nil {
		return nil, Workspace{}, e
	}
	if len(q.Key) > 6600 || compareKey(facts, q.Key) != nil {
		return nil, Workspace{}, ErrInput
	}
	from, to, e := compareBounds(q)
	if e != nil {
		return nil, Workspace{}, e
	}
	w, e := cpamp.FactsView(ctx, facts, "", "", "", q.Key, from, to, func(profileID, windowID string) bool {
		return ex.has("profile:"+hash(profileID), windowID)
	})
	if e != nil {
		return nil, Workspace{}, domainError(ctx, e)
	}
	return facts, w, nil
}

// Compare returns all profiles with their newest measured windows and averages.
func (r *Reader) Compare(ctx context.Context, q CompareQuery, ex Exclusions) (CompareView, error) {
	facts, w, e := r.compareWorkspace(ctx, q, ex)
	if e != nil {
		return CompareView{}, e
	}
	out := CompareView{Version: CompareVersion, FrozenAtMS: facts.Filter.At, ImportedThroughMS: r.ImportedThrough(ctx), Query: q, KeyFiltered: q.Key != "", APIKeys: append([]cpamp.APIKey{}, w.APIKeys...), Profiles: []CompareProfile{}}
	out.PriceBook = ComparePriceBook{ID: w.PriceBook.ID, Policy: w.PriceBook.Policy, CapturedMS: w.PriceBook.Captured}
	if h := w.PriceBook.Historical; h != nil {
		out.PriceBook.Historical = h.Wording
	}
	out.Notes = []string{
		"A window that overlaps the selected dates counts whole.",
		"$ per 1% is measured on the account's shared quota meter, so it is the same for every API key; the key changes the recorded value and its share.",
	}
	keys := cpamp.KeyHashes(q.Key)
	for card, p := range w.Profiles {
		ref := "profile:" + hash(p.ID)
		cp := CompareProfile{Reference: "profile:" + hash(p.ID), Name: p.Name, Provider: p.Provider, Archived: p.Archived, FirstMS: p.First, LastMS: p.Last, Lifetime: compareTotals(p.Lifetime), Selected: compareTotals(p.Selected), Current: []CompareMeter{}, Pools: []ComparePool{}}
		accounts := map[string]bool{}
		for _, m := range p.Periods {
			accounts[m.Account] = true
		}
		cp.Credentials = len(accounts)
		for _, c := range p.Current {
			cp.Current = append(cp.Current, CompareMeter{Kind: c.Kind, Scope: c.Scope, Label: c.Label, UsedPercent: c.Used, ResetMS: c.Reset, LastObservedMS: c.LastObserved, Stale: c.Stale})
		}
		pools := map[string]*ComparePool{}
		order := []string{}
		for _, win := range p.Windows { // already newest first
			key := win.Kind + "|" + win.Scope
			pool := pools[key]
			if pool == nil {
				pool = &ComparePool{Kind: win.Kind, Scope: win.Scope, Main: mainScope(win.Scope), Average: CompareRate{Status: "unavailable", Reason: "No comparable intervals in this pool and selection.", Notes: []string{}}}
				pools[key] = pool
				order = append(order, key)
			}
			pool.Windows++
			excluded := ex.has(ref, win.ID)
			if excluded {
				pool.Excluded++
			}
			if pool.Latest == nil && win.Normalization != nil && win.Normalization.Status == "available" {
				cw := r.usageWindow(facts, card, keys, win, ex[ref])
				pool.Latest = &cw
			} else if pool.Latest == nil && pool.NewestUnmeasured == nil {
				cw := r.usageWindow(facts, card, keys, win, ex[ref])
				pool.NewestUnmeasured = &cw
			}
		}
		for _, g := range p.Rates {
			if pool := pools[g.Kind+"|"+g.Scope]; pool != nil {
				n := g.Normalization
				pool.Average = compareRate(&n)
				pool.AverageModels = modelShares(r.windowsModels(facts, card, keys, n.WindowIDs), 3)
			}
		}
		sort.SliceStable(order, func(i, j int) bool {
			a, b := pools[order[i]], pools[order[j]]
			if a.Kind != b.Kind {
				return a.Kind == "weekly"
			}
			if a.Main != b.Main {
				return a.Main
			}
			return a.Scope < b.Scope
		})
		for _, key := range order {
			pool := pools[key]
			if pool.Latest == nil && pool.NewestUnmeasured != nil {
				// No window in range has a rate: show the newest one with its reason.
				pool.Latest, pool.NewestUnmeasured = pool.NewestUnmeasured, nil
			}
			if pool.AverageModels == nil {
				pool.AverageModels = []ModelShare{}
			}
			cp.Pools = append(cp.Pools, *pool)
		}
		out.Profiles = append(out.Profiles, cp)
	}
	return out, nil
}

// CompareWindows lists one profile pool's windows (newest first) for picking an
// older window. It uses the same facts and selection as Compare.
func (r *Reader) CompareWindows(ctx context.Context, q CompareWindowsQuery, ex Exclusions) (CompareWindowList, error) {
	if q.Kind != "weekly" && q.Kind != "5h" || q.Scope == "" || len(q.Scope) > 256 || len(q.Profile) > 80 {
		return CompareWindowList{}, ErrInput
	}
	facts, w, e := r.compareWorkspace(ctx, q.CompareQuery, ex)
	if e != nil {
		return CompareWindowList{}, e
	}
	keys := cpamp.KeyHashes(q.Key)
	for card, p := range w.Profiles {
		if "profile:"+hash(p.ID) != q.Profile {
			continue
		}
		out := CompareWindowList{Version: CompareVersion, Query: q.CompareQuery, Profile: q.Profile, Kind: q.Kind, Scope: q.Scope, Windows: []CompareWindow{}}
		for _, win := range p.Windows {
			if win.Kind == q.Kind && win.Scope == q.Scope {
				out.Windows = append(out.Windows, r.usageWindow(facts, card, keys, win, ex[q.Profile]))
			}
		}
		return out, nil
	}
	return CompareWindowList{}, ErrInput
}
