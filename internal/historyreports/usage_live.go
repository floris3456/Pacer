package historyreports

// The continuous usage record (docs/usage-unification.md): the prepared facts
// end at H; BuildLive continues every account that Pacer has seen since H with
// the same CPAMP derivation, so the Usage Quota page has one window list.

import (
	"context"
	"sort"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// liveTail is how much imported evidence before H a rebuilt account reads, so a
// window that straddles H (and its neighbours) is derived from all its readings.
const liveTail = 14 * 24 * time.Hour

type LiveInput struct {
	Now      time.Time
	Native   UsageNative
	Readings []NativeReading
	Source   string // Pacer's producer source ID (native account references)
}

// LiveStatus describes the continuous record for the page's technical details.
type LiveStatus struct {
	State             string    `json:"state"` // starting, off, waiting, live, error
	ImportedThroughMS int64     `json:"imported_through_ms,omitempty"`
	BuiltAtMS         int64     `json:"built_at_ms,omitempty"`
	Stats             LiveStats `json:"stats"`
	Error             string    `json:"error,omitempty"`
}

type LiveStats struct {
	HMS             int64 `json:"h_ms"`
	AtMS            int64 `json:"at_ms"`
	Requests        int   `json:"native_requests"`
	RequestsBeforeH int   `json:"native_requests_before_h"`
	Readings        int   `json:"native_readings"`
	ReadingsSkipped int   `json:"native_readings_skipped"`
	Ambiguous       int   `json:"ambiguous_account"`
	Accounts        int   `json:"accounts_rebuilt"`
	NewAccounts     int   `json:"new_accounts"`
	Windows         int   `json:"live_windows"`
	ElapsedMS       int64 `json:"elapsed_ms"`
}

type liveTailEvidence struct {
	events []cpamp.Event
	obs    []cpamp.Observation
}

// tail returns an account's imported evidence from H − liveTail, read once.
func (r *Reader) tail(ctx context.Context, facts *cpamp.QuotaFacts, account string) (liveTailEvidence, error) {
	r.liveMu.Lock()
	t, ok := r.liveTails[account]
	r.liveMu.Unlock()
	if ok {
		return t, nil
	}
	events, obs, err := r.service.AccountEvidence(ctx, facts, account)
	if err != nil {
		return t, domainError(ctx, err)
	}
	from := facts.Filter.At - liveTail.Milliseconds()
	t = liveTailEvidence{events: []cpamp.Event{}, obs: []cpamp.Observation{}}
	for _, e := range events {
		if e.Started >= from {
			t.events = append(t.events, e)
		}
	}
	for _, o := range obs {
		if o.Observed >= from {
			t.obs = append(t.obs, o)
		}
	}
	r.liveMu.Lock()
	if r.liveTails == nil {
		r.liveTails = map[string]liveTailEvidence{}
	}
	r.liveTails[account] = t
	r.liveMu.Unlock()
	return t, nil
}

// BuildLive continues the prepared facts with Pacer's own requests and meter
// readings from H on. Before H everything stays imported; from H on only
// Pacer's evidence counts. A rebuilt account's windows that reset after H are
// replaced by windows derived from its imported tail plus native evidence.
func (r *Reader) BuildLive(ctx context.Context, in LiveInput) (*cpamp.QuotaFacts, LiveStats, error) {
	started := time.Now()
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return nil, LiveStats{}, err
	}
	h := frozen.Filter.At
	stats := LiveStats{HMS: h, AtMS: in.Now.UnixMilli()}
	if stats.AtMS < h {
		return nil, stats, ErrInput
	}
	known, err := r.service.Accounts(ctx)
	if err != nil {
		return nil, stats, domainError(ctx, err)
	}
	accounts := map[string]cpamp.Account{}
	for _, a := range known {
		accounts[a.ID] = a
	}
	native := map[string][]cpamp.Event{}
	readings := map[string][]cpamp.Observation{}
	provider := map[string]string{}
	label := map[string]string{} // a new account is labelled with Pacer's own account name
	for _, e := range in.Native.Events {
		if e.StartedAt.UnixMilli() < h {
			stats.RequestsBeforeH++
			continue
		}
		account := ForwardAccountRef(e.SourceID, e.Event.Provider, e.Event.Account)
		cert, ambiguous := originForNative(in.Native.Origins, e)
		if ambiguous != nil {
			stats.Ambiguous++
			continue
		}
		if cert != nil {
			account = cert.LegacyAccount
		}
		ev := e.Event
		ev.Account = account
		native[account] = append(native[account], ev)
		provider[account] = ev.Provider
		label[account] = e.Event.Account
		stats.Requests++
	}
	for _, rd := range in.Readings {
		if rd.At.UnixMilli() < h {
			stats.ReadingsSkipped++
			continue
		}
		account, ok := readingAccount(in.Native.Origins, in.Source, rd)
		if !ok {
			stats.Ambiguous++
			continue
		}
		o, ok := nativeObservation(rd, account)
		if !ok {
			stats.ReadingsSkipped++
			continue
		}
		readings[account] = append(readings[account], o)
		provider[account] = rd.Provider
		label[account] = rd.Account
		stats.Readings++
	}
	ids := make([]string, 0, len(provider))
	for id := range provider {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Native request IDs continue after the imported ones (never valued as the
	// frozen cohort), in one start order across accounts.
	all := []*cpamp.Event{}
	for _, id := range ids {
		for i := range native[id] {
			all = append(all, &native[id][i])
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Started != all[j].Started {
			return all[i].Started < all[j].Started
		}
		return all[i].Hash < all[j].Hash
	})
	for i, e := range all {
		e.ID = frozen.Filter.Through + int64(i) + 1
	}

	frame := cpamp.QuotaFacts{Version: frozen.Version, Filter: frozen.Filter, Config: frozen.Config, PriceBook: frozen.PriceBook, APIKeys: frozen.APIKeys}
	frame.Filter.At = stats.AtMS
	out := frame
	out.Credentials = append([]cpamp.ProfileCredential{}, frozen.Credentials...)
	out.UnassignedPeriods = append([]cpamp.ProfileMembership{}, frozen.UnassignedPeriods...)
	out.OrderSensitive = frozen.OrderSensitive
	out.Buckets = append([]cpamp.FactBucket{}, frozen.Buckets...)
	rebuilt := map[string]bool{}
	live := []cpamp.FactWindow{}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return nil, stats, err
		}
		events := native[id]
		sort.SliceStable(events, func(i, j int) bool {
			if events[i].Started != events[j].Started {
				return events[i].Started < events[j].Started
			}
			return events[i].ID < events[j].ID
		})
		cpamp.ValueEvents(events, frame.PriceBook)
		obs := readings[id]
		sort.SliceStable(obs, func(i, j int) bool { return obs[i].Observed < obs[j].Observed })
		a, imported := accounts[id]
		var t liveTailEvidence
		if imported {
			if t, err = r.tail(ctx, frozen, id); err != nil {
				return nil, stats, err
			}
		} else {
			a = cpamp.Account{ID: id, Provider: provider[id], Label: label[id]}
			stats.NewAccounts++
		}
		windows := cpamp.AccountFactsFor(&frame, a, append(append([]cpamp.Event{}, t.events...), events...), append(append([]cpamp.Observation{}, t.obs...), obs...))
		buckets := cpamp.AccountFactsFor(&frame, a, events, nil)
		rebuilt[id] = true
		stats.Accounts++
		for _, w := range windows.Windows {
			if w.Base.Reset > h {
				live = append(live, w)
			}
		}
		out.Buckets = append(out.Buckets, buckets.Buckets...)
		out.OrderSensitive = out.OrderSensitive || buckets.OrderSensitive
		if windows.NextTransition != 0 && (out.NextTransition == 0 || windows.NextTransition < out.NextTransition) {
			out.NextTransition = windows.NextTransition
		}
		if buckets.Unassigned {
			present := false
			for _, p := range out.UnassignedPeriods {
				present = present || p.Account == id
			}
			if !present {
				out.UnassignedPeriods = append(out.UnassignedPeriods, cpamp.ProfileMembership{Account: id})
			}
		}
		found := false
		for i := range out.Credentials {
			if c := &out.Credentials[i]; c.Account.ID == id {
				found = true
				c.Requests += int64(len(events))
				if len(events) > 0 {
					if c.First == 0 {
						c.First = events[0].Started
					}
					c.Last = max(c.Last, events[len(events)-1].Started)
				}
			}
		}
		if !found {
			c := cpamp.ProfileCredential{Account: a, Status: "unknown", Requests: int64(len(events))}
			if len(events) > 0 {
				c.First, c.Last = events[0].Started, events[len(events)-1].Started
			}
			out.Credentials = append(out.Credentials, c)
		}
	}
	out.Windows = []cpamp.FactWindow{}
	for _, w := range frozen.Windows {
		if rebuilt[w.Account] && w.Base.Reset > h {
			continue
		}
		out.Windows = append(out.Windows, w)
	}
	out.Windows = append(out.Windows, live...)
	stats.Windows = len(live)
	stats.ElapsedMS = time.Since(started).Milliseconds()
	return &out, stats, nil
}

// UseLive makes the usage views read live facts (nil returns to the prepared
// facts). The exact workspace path always keeps the prepared facts.
func (r *Reader) UseLive(q *cpamp.QuotaFacts) {
	r.liveMu.Lock()
	r.live = q
	r.liveMu.Unlock()
}

// usageFacts are the live facts when set, otherwise the prepared facts.
func (r *Reader) usageFacts(ctx context.Context) (*cpamp.QuotaFacts, error) {
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return nil, err
	}
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.live != nil {
		return r.live, nil
	}
	return frozen, nil
}

// ImportedThrough is H, the end of the imported history (0 when unknown).
func (r *Reader) ImportedThrough(ctx context.Context) int64 {
	frozen, err := r.quotaFacts(ctx)
	if err != nil {
		return 0
	}
	return frozen.Filter.At
}
