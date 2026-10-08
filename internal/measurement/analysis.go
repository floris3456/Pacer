package measurement

import (
	"math"
	"sort"
	"time"
)

type IntervalEstimate struct {
	From             time.Time `json:"from"`
	Through          time.Time `json:"through"`
	FromObservation  string    `json:"from_observation"`
	ToObservation    string    `json:"to_observation"`
	FromSource       string    `json:"from_source"`
	ToSource         string    `json:"to_source"`
	DeltaPercent     float64   `json:"delta_percent"`
	Requests         int       `json:"requests"`
	CrossingRequests int       `json:"crossing_requests"`
	Value            Value     `json:"value"`
	Coverage         Coverage  `json:"coverage"`
	Reasons          []string  `json:"reasons,omitempty"`
}

type WindowEstimate struct {
	CycleRef
	From          time.Time          `json:"from"`
	Through       time.Time          `json:"through"`
	Completed     bool               `json:"completed"`
	Closure       *Closure           `json:"closure,omitempty"`
	DeltaPercent  float64            `json:"delta_percent"`
	Value         Value              `json:"value"`
	Intervals     []IntervalEstimate `json:"intervals"`
	Eligible      bool               `json:"eligible"`
	USDPerPercent float64            `json:"usd_per_percent"`
	Reasons       []string           `json:"reasons,omitempty"`
}

type Ratio struct {
	Known         bool     `json:"known"`
	USDPerPercent float64  `json:"usd_per_percent"`
	CycleIDs      []string `json:"cycle_ids,omitempty"`
	Method        string   `json:"method"`
}

type Report struct {
	Provider         string           `json:"provider"`
	AccountID        string           `json:"account_id"`
	Scope            string           `json:"scope"`
	Since            time.Time        `json:"since"`
	AsOf             time.Time        `json:"as_of"`
	PriceBookID      string           `json:"price_book_id"`
	ValuationVersion string           `json:"valuation_version"`
	EvidenceID       string           `json:"evidence_id"`
	Coverage         Coverage         `json:"coverage"`
	Windows          []WindowEstimate `json:"windows"`
	FiveHour         Ratio            `json:"five_hour"`
	SevenDay         Ratio            `json:"seven_day"`
	Stretches        []Stretch        `json:"stretches"`
	WarmBurn         Burn             `json:"warm_burn"`
}

func analyze(e evidence, q Query) Report {
	r := Report{Provider: q.Provider, AccountID: q.AccountID, Scope: q.Scope, Since: q.Since, AsOf: q.AsOf, PriceBookID: e.Book.ID, ValuationVersion: ValuationVersion, EvidenceID: evidenceID(e, q), Coverage: coverage(e, q.AccountID, q.Since, q.AsOf)}
	groups := map[string][]Observation{}
	for _, o := range e.Observations {
		id := key(o.ref())
		groups[id] = append(groups[id], o)
	}
	for id, observations := range groups {
		r.Windows = append(r.Windows, measureWindow(e, q, observations, e.Closures[id]))
	}
	sort.Slice(r.Windows, func(i, j int) bool {
		if !r.Windows[i].Through.Equal(r.Windows[j].Through) {
			return r.Windows[i].Through.Before(r.Windows[j].Through)
		}
		return r.Windows[i].CycleID < r.Windows[j].CycleID
	})
	r.FiveHour, r.SevenDay = chooseRatios(r.Windows)
	r.Stretches = buildStretches(e, q)
	r.WarmBurn = accountBurn(r.Stretches, q.AccountID, q.AsOf)
	// Missing delivery in the longest supported protection horizon could hide
	// an entire warm chat. An empty observed list is not evidence of zero burn.
	recent := coverage(e, q.AccountID, q.AsOf.Add(-time.Hour), q.AsOf)
	if !recent.Observed {
		r.WarmBurn.Known, r.WarmBurn.Bounded = false, false
		r.WarmBurn.Reasons = unique(append(r.WarmBurn.Reasons, recent.Reasons...))
	}
	return r
}

func matchingModel(r Record, models []string) bool {
	if len(models) == 0 {
		return true
	}
	for _, m := range models {
		if m == r.Attribution.Model {
			return true
		}
	}
	return false
}

func measureWindow(e evidence, q Query, obs []Observation, c Closure) WindowEstimate {
	w := WindowEstimate{CycleRef: obs[0].ref(), From: obs[0].At, Through: obs[len(obs)-1].At}
	if !c.At.IsZero() && !c.At.After(q.AsOf) {
		w.Completed = true
		w.Closure = &c
		if c.At.Before(w.Through) {
			w.Reasons = append(w.Reasons, "observations_after_cycle_closure")
		}
	}
	if len(obs) < 2 {
		w.Reasons = append(w.Reasons, "insufficient_observations")
	}
	// Assign each attempt only to intervals it overlaps. Avoid an all-records
	// scan for every quota sample; normal requests visit one interval each.
	type priced struct {
		index int
		value Value
	}
	byInterval := make([][]priced, len(obs)-1)
	for index, r := range e.Records {
		if r.Attribution.AccountID != q.AccountID || !matchingModel(r, obs[0].Models) {
			continue
		}
		first := sort.Search(len(obs)-1, func(i int) bool { return obs[i+1].At.After(r.StartedAt) })
		if first == len(obs)-1 {
			continue
		}
		v := ValueRecord(r, e.Book)
		for i := first; i < len(obs)-1; i++ {
			if r.StartedAt.Before(obs[i].At) && !r.EndedAt.IsZero() && !r.EndedAt.After(obs[i].At) {
				break
			}
			byInterval[i] = append(byInterval[i], priced{index, v})
		}
	}
	for i := 1; i < len(obs); i++ {
		a, b := obs[i-1], obs[i]
		if a.At.Equal(b.At) && a.UsedPercent == b.UsedPercent && a.Usable && b.Usable {
			continue
		}
		in := IntervalEstimate{From: a.At, Through: b.At, FromObservation: a.ID, ToObservation: b.ID, FromSource: a.Source, ToSource: b.Source, DeltaPercent: b.UsedPercent - a.UsedPercent, Coverage: coverage(e, q.AccountID, a.At, b.At)}
		if !a.Usable || !b.Usable {
			in.Reasons = append(in.Reasons, "unusable_observation")
		}
		if !b.At.After(a.At) || in.DeltaPercent < 0 {
			in.Reasons = append(in.Reasons, "meter_correction_or_conflict")
		}
		if !in.Coverage.Observed {
			in.Reasons = append(in.Reasons, in.Coverage.Reasons...)
		}
		for _, p := range byInterval[i-1] {
			r := e.Records[p.index]
			in.Requests++
			v := p.value
			if r.StartedAt.Before(a.At) || r.EndedAt.IsZero() || r.EndedAt.After(b.At) {
				in.CrossingRequests++
				// Boundary allocation is independent of TTL valuation uncertainty.
				v.LowNanoUSD = 0
				in.Reasons = append(in.Reasons, "request_crosses_observation_boundary")
			}
			in.Value.add(v)
		}
		if in.DeltaPercent > 0 && in.Requests == 0 {
			in.Reasons = append(in.Reasons, "outside_usage_without_cpa_requests")
		}
		if !in.Value.Exact() {
			in.Reasons = append(in.Reasons, "unresolved_value")
		}
		in.Reasons = unique(in.Reasons)
		w.Intervals = append(w.Intervals, in)
		w.DeltaPercent += in.DeltaPercent
		w.Value.add(in.Value)
		w.Reasons = append(w.Reasons, in.Reasons...)
	}
	if w.DeltaPercent < 10 {
		w.Reasons = append(w.Reasons, "below_ten_point_delta")
	}
	if w.Value.LowNanoUSD <= 0 {
		w.Reasons = append(w.Reasons, "no_positive_measured_value")
	}
	w.Reasons = unique(w.Reasons)
	w.Eligible = len(w.Reasons) == 0
	if w.Eligible {
		w.USDPerPercent = w.Value.LowUSD() / w.DeltaPercent
	}
	return w
}

func chooseRatios(windows []WindowEstimate) (five, seven Ratio) {
	five.Method = "unmeasured"
	seven.Method = "unmeasured"
	var f []WindowEstimate
	var weekly []WindowEstimate
	for _, w := range windows {
		if w.Duration == 5*time.Hour && w.Completed && w.Eligible {
			f = append(f, w)
		}
		if w.Duration == 7*24*time.Hour {
			weekly = append(weekly, w)
		}
	}
	if len(f) > 2 {
		f = f[len(f)-2:]
	}
	if len(f) > 0 {
		five.Known = true
		five.Method = "last_completed_window"
		five.USDPerPercent = f[0].USDPerPercent
		five.CycleIDs = []string{f[0].CycleID}
		if len(f) == 2 {
			five.CycleIDs = append(five.CycleIDs, f[1].CycleID)
			low, high := math.Min(f[0].USDPerPercent, f[1].USDPerPercent), math.Max(f[0].USDPerPercent, f[1].USDPerPercent)
			if high > low*1.5 {
				five.USDPerPercent = low
				five.Method = "lower_of_divergent_last_two"
			} else {
				five.USDPerPercent = (f[0].Value.LowUSD() + f[1].Value.LowUSD()) / (f[0].DeltaPercent + f[1].DeltaPercent)
				five.Method = "delta_weighted_last_two"
			}
		}
	}
	for i := len(weekly) - 1; i >= 0; i-- {
		w := weekly[i]
		if !w.Eligible || !w.Completed && i != len(weekly)-1 {
			continue
		}
		seven = Ratio{Known: true, USDPerPercent: w.USDPerPercent, CycleIDs: []string{w.CycleID}, Method: "last_completed_window"}
		if !w.Completed {
			seven.Method = "current_window_ten_points"
		}
		break
	}
	return five, seven
}
