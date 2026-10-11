package historyreports

// Usage views on top of Compare: per-window model mix, exclusions and Inspect.
// Everything here reads the in-memory quota facts only.

import (
	"context"
	"sort"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// factsIndex maps window ID -> fact window for the loaded facts.
func (r *Reader) factsIndex(facts *cpamp.QuotaFacts) map[string]*cpamp.FactWindow {
	r.factsMu.Lock()
	defer r.factsMu.Unlock()
	if r.factsWindows == nil || r.factsWindowsFor != facts {
		r.factsWindows = make(map[string]*cpamp.FactWindow, len(facts.Windows))
		for i := range facts.Windows {
			r.factsWindows[facts.Windows[i].Base.ID] = &facts.Windows[i]
		}
		r.factsWindowsFor = facts
	}
	return r.factsWindows
}

func hasCardIndex(cards []int, i int) bool {
	for _, c := range cards {
		if c == i {
			return true
		}
	}
	return false
}

func keyIncluded(keys []string, hash string) bool {
	if keys == nil {
		return true
	}
	for _, k := range keys {
		if k == hash {
			return true
		}
	}
	return false
}

// windowsModels sums the profile's (card's) selected totals per model over the
// given windows, honoring the key selector.
func (r *Reader) windowsModels(facts *cpamp.QuotaFacts, card int, keys []string, windowIDs []string) map[string]cpamp.ProfileTotals {
	index := r.factsIndex(facts)
	out := map[string]cpamp.ProfileTotals{}
	for _, id := range windowIDs {
		fw := index[id]
		if fw == nil {
			continue
		}
		for _, sel := range fw.Selections {
			if !hasCardIndex(sel.Cards, card) || !keyIncluded(keys, sel.Key) {
				continue
			}
			t := out[sel.Model]
			t.AddTotals(sel.Totals)
			out[sel.Model] = t
		}
	}
	return out
}

func valueMid(t cpamp.ProfileTotals) float64 {
	if !t.Valued {
		return 0
	}
	return (float64(t.Value.Low) + float64(t.Value.High)) / 2
}

// modelShares turns per-model totals into shares of value (tokens when no value
// is known), largest first; models beyond top are folded into "Other".
func modelShares(models map[string]cpamp.ProfileTotals, top int) []ModelShare {
	out := []ModelShare{}
	weight := func(t cpamp.ProfileTotals) float64 { return valueMid(t) }
	total := 0.0
	for _, t := range models {
		total += weight(t)
	}
	if total <= 0 {
		weight = func(t cpamp.ProfileTotals) float64 { return float64(t.Tokens.Total) }
		for _, t := range models {
			total += weight(t)
		}
	}
	if total <= 0 {
		if top != 0 {
			return out
		}
		total = 1 // nothing to share: the full list still names every model, at 0%
	}
	for name, t := range models {
		// The full list (top 0, Inspect) keeps models with no share, such as
		// unpriced or failed-only ones, so its rows add up to the totals.
		if w := weight(t); w > 0 || top == 0 {
			out = append(out, ModelShare{Model: name, SharePercent: w * 100 / total})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SharePercent != out[j].SharePercent {
			return out[i].SharePercent > out[j].SharePercent
		}
		return out[i].Model < out[j].Model
	})
	if top > 0 && len(out) > top+1 {
		other := ModelShare{Model: "Other"}
		for _, m := range out[top:] {
			other.SharePercent += m.SharePercent
		}
		out = append(out[:top:top], other)
	}
	return out
}

func (r *Reader) usageWindow(facts *cpamp.QuotaFacts, card int, keys []string, w cpamp.QuotaUsageWindow, excluded map[string]string) CompareWindow {
	cw := compareWindow(w)
	cw.Models = modelShares(r.windowsModels(facts, card, keys, []string{w.ID}), 3)
	cw.Verified = r.verified(w)
	if note, ok := excluded[w.ID]; ok {
		cw.Excluded, cw.Note = true, note
	}
	return cw
}

// usageCard is one card on the usage page: a profile, or the accounts that are
// in no profile ("unassigned", only when it has usage). index is the facts
// card index (profiles in config order, then unassigned).
type usageCard struct {
	index     int
	reference string
	usage     cpamp.SavedProfileUsage
}

const unassignedReference = "unassigned"

func usageCards(w Workspace) []usageCard {
	out := make([]usageCard, 0, len(w.Profiles)+1)
	for i, p := range w.Profiles {
		out = append(out, usageCard{index: i, reference: "profile:" + hash(p.ID), usage: p})
	}
	if u := w.Unassigned; len(u.Windows) > 0 || u.Lifetime.Requests > 0 {
		u.Name = "Not in a profile"
		out = append(out, usageCard{index: len(w.Profiles), reference: unassignedReference, usage: u})
	}
	return out
}

const unassignedReason = "Accounts in no profile have no $ per 1%. Add the account to a profile to measure it."

// unassignedRates explains a missing rate on the "Not in a profile" card in
// terms of what to do; measured numbers are never changed.
func unassignedRates(pool *ComparePool) {
	for _, r := range []*CompareRate{&pool.Average, rateOf(pool.Latest), rateOf(pool.NewestUnmeasured)} {
		if r != nil && r.Status != "available" {
			r.Reason = unassignedReason
		}
	}
}

func rateOf(w *CompareWindow) *CompareRate {
	if w == nil {
		return nil
	}
	return &w.Rate
}

// ---- Inspect ----

type InspectQuery struct {
	CompareQuery
	Profile string `json:"profile"`
	Kind    string `json:"kind"`
	Scope   string `json:"scope"`
	// Window selects one window; empty inspects the average.
	Window string `json:"window,omitempty"`
}

type InspectTokens struct {
	Input      int64 `json:"input"` // uncached input
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
	Output     int64 `json:"output"`
	Total      int64 `json:"total"`
}

type InspectCost struct {
	InputNanoUSD          int64 `json:"input_nano_usd"`
	CacheWriteLowNanoUSD  int64 `json:"cache_write_low_nano_usd"`
	CacheWriteHighNanoUSD int64 `json:"cache_write_high_nano_usd"`
	CacheReadNanoUSD      int64 `json:"cache_read_nano_usd"`
	OutputNanoUSD         int64 `json:"output_nano_usd"`
}

type InspectTotals struct {
	Requests            int64         `json:"requests"`
	Failed              int64         `json:"failed"`
	FailedPercent       *float64      `json:"failed_percent"`
	AvgTokensPerRequest *float64      `json:"avg_tokens_per_request"`
	Tokens              InspectTokens `json:"tokens"`
	CacheHitPercent     *float64      `json:"cache_hit_percent"`
	Valued              bool          `json:"valued"`
	ValueLowNanoUSD     int64         `json:"value_low_nano_usd"`
	ValueHighNanoUSD    int64         `json:"value_high_nano_usd"`
	MissingPrice        int64         `json:"missing_price_requests"`
	Cost                InspectCost   `json:"cost"`
	// Cache writes by lifetime: 5-minute, 1-hour, unknown, and unknown counted as 5m by rule.
	CacheWrite5m         int64 `json:"cache_write_5m"`
	CacheWrite1h         int64 `json:"cache_write_1h"`
	CacheWriteUnresolved int64 `json:"cache_write_unresolved"`
	CacheWriteAssumed5m  int64 `json:"cache_write_assumed_5m"`
}

type InspectModel struct {
	Model        string  `json:"model"`
	SharePercent float64 `json:"share_percent"`
	InspectTotals
}

type InspectKey struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	SharePercent float64 `json:"share_percent"`
	Requests     int64   `json:"requests"`
}

// InspectPerPercent: what 1% of quota bought at the account's request mix in
// the inspected window(s): rate midpoint × account usage ÷ account value.
type InspectPerPercent struct {
	Requests     float64 `json:"requests"`
	OutputTokens float64 `json:"output_tokens"`
	TotalTokens  float64 `json:"total_tokens"`
}

type InspectMeter struct {
	StartPercent *float64 `json:"start_percent"`
	EndPercent   *float64 `json:"end_percent"`
	DeltaPP      float64  `json:"delta_pp"`
	FromMS       int64    `json:"from_ms"`
	ThroughMS    int64    `json:"through_ms"`
	Readings     int64    `json:"readings"`
	Samples      int      `json:"samples"`
	Skipped      int      `json:"skipped_windows"`
	Notes        []string `json:"notes"`
}

type InspectView struct {
	Version     string             `json:"version"`
	Query       InspectQuery       `json:"query"`
	ProfileName string             `json:"profile_name"`
	Provider    string             `json:"provider"`
	Mode        string             `json:"mode"` // window or average
	Window      *CompareWindow     `json:"window,omitempty"`
	Windows     int                `json:"windows"` // windows the numbers cover
	Excluded    int                `json:"excluded"`
	Rate        CompareRate        `json:"rate"`
	Totals      InspectTotals      `json:"totals"`
	Models      []InspectModel     `json:"models"`
	Keys        []InspectKey       `json:"keys"`
	PerPercent  *InspectPerPercent `json:"per_percent"`
	Meter       InspectMeter       `json:"meter"`
}

func pctOf(part, whole float64) *float64 {
	if whole <= 0 {
		return nil
	}
	v := part * 100 / whole
	return &v
}

func inspectTotals(t cpamp.ProfileTotals) InspectTotals {
	out := InspectTotals{Requests: t.Requests, Failed: t.Failed, Valued: t.Valued, ValueLowNanoUSD: t.Value.Low, ValueHighNanoUSD: t.Value.High, MissingPrice: t.Value.Missing}
	out.Tokens = InspectTokens{Input: max(int64(0), t.Tokens.Input-t.Tokens.Cached-t.Tokens.CacheCreation), CacheWrite: t.Tokens.CacheCreation, CacheRead: t.Tokens.Cached, Output: t.Tokens.Output, Total: t.Tokens.Total}
	out.FailedPercent = pctOf(float64(t.Failed), float64(t.Requests))
	if t.Requests > 0 {
		v := float64(t.Tokens.Total) / float64(t.Requests)
		out.AvgTokensPerRequest = &v
	}
	out.CacheHitPercent = pctOf(float64(t.Tokens.Cached), float64(t.Tokens.Input))
	c := t.Value.Costs
	out.Cost = InspectCost{InputNanoUSD: c.Input, CacheWriteLowNanoUSD: c.WriteLow, CacheWriteHighNanoUSD: c.WriteHigh, CacheReadNanoUSD: c.Cached, OutputNanoUSD: c.Output}
	v := t.Value.Tokens
	out.CacheWrite5m, out.CacheWrite1h, out.CacheWriteUnresolved, out.CacheWriteAssumed5m = v.Write5m, v.Write1h, v.WriteUnknown, v.WriteAssumed5m
	return out
}

func keySelectorHashes(id string) []string {
	if id == cpamp.UnknownAPIKey {
		return []string{""}
	}
	if strings.HasPrefix(id, "keys:") {
		return strings.Split(strings.TrimPrefix(id, "keys:"), ",")
	}
	return []string{id}
}

// Inspect breaks down one profile pool, under the same filters as Compare, for
// either one window (Window set) or the windows its average used.
func (r *Reader) Inspect(ctx context.Context, q InspectQuery, ex Exclusions) (InspectView, error) {
	if !validKind(q.Kind) || q.Scope == "" || len(q.Scope) > 256 || len(q.Profile) > 80 || len(q.Window) > 128 {
		return InspectView{}, ErrInput
	}
	facts, w, e := r.compareWorkspace(ctx, q.CompareQuery, ex)
	if e != nil {
		return InspectView{}, e
	}
	keys := cpamp.KeyHashes(q.Key)
	index := r.factsIndex(facts)
	for _, c := range usageCards(w) {
		card, p, ref := c.index, c.usage, c.reference
		if ref != q.Profile {
			continue
		}
		out := InspectView{Version: CompareVersion, Query: q, ProfileName: p.Name, Provider: p.Provider, Models: []InspectModel{}, Keys: []InspectKey{}, Meter: InspectMeter{Notes: []string{}}}
		var windowIDs []string
		var account cpamp.ProfileTotals
		if q.Window != "" {
			out.Mode = "window"
			fw := index[q.Window]
			if fw == nil || fw.Base.Kind != q.Kind || fw.Base.Scope != q.Scope {
				return InspectView{}, ErrInput
			}
			var normalization *cpamp.ProfileNormalization
			found := false
			for _, fc := range fw.Cards {
				if fc.Index == card {
					found, normalization = true, fc.Normalization
				}
			}
			if !found {
				return InspectView{}, ErrInput
			}
			base := fw.Base
			base.Normalization = normalization
			cw := r.usageWindow(facts, card, keys, base, ex[ref])
			out.Window, out.Rate, windowIDs, out.Windows = &cw, cw.Rate, []string{q.Window}, 1
			account = fw.Base.AccountUsage
			out.Meter = InspectMeter{StartPercent: cw.Rate.StartPercent, EndPercent: cw.Rate.EndPercent, DeltaPP: cw.Rate.DeltaPP, FromMS: cw.Rate.FromMS, ThroughMS: cw.Rate.ThroughMS, Readings: fw.Readings, Samples: cw.Rate.Samples, Skipped: cw.Rate.Skipped, Notes: cw.Rate.Notes}
		} else {
			out.Mode = "average"
			out.Rate = CompareRate{Status: "unavailable", Reason: "No comparable intervals in this pool and selection.", Notes: []string{}}
			for _, g := range p.Rates {
				if g.Kind == q.Kind && g.Scope == q.Scope {
					n := g.Normalization
					out.Rate = compareRate(&n)
					windowIDs = append(windowIDs, n.WindowIDs...)
				}
			}
			if len(windowIDs) == 0 { // nothing measured: still show the mix of every counted window
				ids, filtered := q.SelectedWindows[ref]
				keep := latestProfileWindows(p)
				for _, id := range ids {
					keep[id] = true
				}
				for _, win := range p.Windows {
					if win.Kind == q.Kind && win.Scope == q.Scope && !ex.has(ref, win.ID) && (!(filtered || q.LatestOnly) || keep[win.ID]) {
						windowIDs = append(windowIDs, win.ID)
					}
				}
			}
			for _, win := range p.Windows {
				if win.Kind == q.Kind && win.Scope == q.Scope && ex.has(ref, win.ID) {
					out.Excluded++
				}
			}
			out.Windows = len(windowIDs)
			out.Meter = InspectMeter{DeltaPP: out.Rate.DeltaPP, FromMS: out.Rate.FromMS, ThroughMS: out.Rate.ThroughMS, Samples: out.Rate.Samples, Skipped: out.Rate.Skipped, Notes: out.Rate.Notes}
			for _, id := range windowIDs {
				if fw := index[id]; fw != nil {
					account.AddTotals(fw.Base.AccountUsage)
					out.Meter.Readings += fw.Readings
				}
			}
		}
		models := r.windowsModels(facts, card, keys, windowIDs)
		var total cpamp.ProfileTotals
		for _, t := range models {
			total.AddTotals(t)
		}
		out.Totals = inspectTotals(total)
		if out.Window != nil {
			out.Window.Selected = compareTotals(total)
			if index[q.Window].TokensTotal > 0 {
				share := float64(total.Tokens.Total) * 100 / float64(index[q.Window].TokensTotal)
				out.Window.TokenSharePercent = &share
			}
		}
		for _, share := range modelShares(models, 0) {
			out.Models = append(out.Models, InspectModel{Model: share.Model, SharePercent: share.SharePercent, InspectTotals: inspectTotals(models[share.Model])})
		}
		if keys == nil { // every key: show each key's share of this profile's value
			byKey := map[string]cpamp.ProfileTotals{}
			labels := map[string]string{}
			owner := map[string]string{}
			for _, k := range facts.APIKeys {
				labels[k.ID] = k.Label
				for _, h := range keySelectorHashes(k.ID) {
					owner[h] = k.ID
				}
			}
			for _, id := range windowIDs {
				fw := index[id]
				if fw == nil {
					continue
				}
				for _, sel := range fw.Selections {
					if !hasCardIndex(sel.Cards, card) {
						continue
					}
					k := owner[sel.Key]
					if k == "" {
						k = sel.Key
					}
					t := byKey[k]
					t.AddTotals(sel.Totals)
					byKey[k] = t
				}
			}
			for _, share := range modelShares(byKey, 0) {
				label := labels[share.Model]
				if label == "" {
					label = "Key " + share.Model[:min(12, len(share.Model))]
				}
				out.Keys = append(out.Keys, InspectKey{Key: share.Model, Label: label, SharePercent: share.SharePercent, Requests: byKey[share.Model].Requests})
			}
		}
		if rate := out.Rate.USDPerPercent; rate != nil && out.Rate.Status == "available" {
			if value := valueMid(account) / 1e9; value > 0 {
				mid := (rate.Low + rate.High) / 2
				scale := mid / value
				out.PerPercent = &InspectPerPercent{Requests: float64(account.Requests) * scale, OutputTokens: float64(account.Tokens.Output) * scale, TotalTokens: float64(account.Tokens.Total) * scale}
			}
		}
		return out, nil
	}
	return InspectView{}, ErrInput
}
