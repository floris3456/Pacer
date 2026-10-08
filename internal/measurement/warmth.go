package measurement

import (
	"math"
	"sort"
	"time"
)

type Stretch struct {
	ChatRef
	TTL                TTL       `json:"ttl"`
	PrefixRef          string    `json:"prefix_ref,omitempty"`
	StartedAt          time.Time `json:"started_at"`
	LastRequestAt      time.Time `json:"last_request_at"`
	ProtectedUntil     time.Time `json:"protected_until"`
	ClosedAt           time.Time `json:"closed_at"`
	Requests           int       `json:"requests"`
	QualifyingRequests int       `json:"qualifying_requests"`
	Value              Value     `json:"value"`
	Coverage           Coverage  `json:"coverage"`
	Current            bool      `json:"current"`
	UnknownWarmth      bool      `json:"unknown_warmth"`
	InFlight           bool      `json:"in_flight"`
	LeftCensored       bool      `json:"left_censored"`
	PriorEligible      bool      `json:"prior_eligible"`
	Reasons            []string  `json:"reasons,omitempty"`
}

func (s Stretch) Warm(at time.Time) bool {
	return s.InFlight || s.Current && (s.UnknownWarmth || at.Before(s.ProtectedUntil))
}

func baseChat(r Record) string {
	c := r.Chat()
	c.AccountID = ""
	c.ContextEpoch = ""
	if c.CallerID == "" || c.SessionID == "" {
		return "unattributed:" + r.EventID
	}
	return key(c)
}

func protectionTTL(r Record) TTL {
	t := r.Activity.ProtectionTTL
	if r.Policy.EffectiveTTL.Duration() > t.Duration() {
		t = r.Policy.EffectiveTTL
	}
	return t
}

func qualifies(r Record) bool {
	_, valid := normalized(r.Usage)
	return r.Final && r.Success && r.Activity.PrimaryPrefix && valid && (r.Usage.CacheRead > 0 || r.Usage.CacheCreate > 0) && (r.Kind == "primary" || r.Kind == "warming")
}

func buildStretches(e evidence, q Query) []Stretch {
	var out []Stretch
	current := map[string]int{}
	for _, r := range e.Records {
		if r.Kind != "primary" && r.Kind != "warming" {
			continue
		}
		if r.Kind == "warming" && !r.Activity.PrimaryPrefix {
			continue
		}
		id := baseChat(r)
		idx, found := current[id]
		ttl := protectionTTL(r)
		compatible := found && out[idx].ChatRef == r.Chat() && out[idx].TTL == ttl
		// A warming call on another binding/epoch never rebinds the primary chat.
		if r.Kind == "warming" && found && !compatible {
			continue
		}
		if r.Kind == "warming" && found && out[idx].PrefixRef != "" && r.Activity.PrefixRef != "" && out[idx].PrefixRef != r.Activity.PrefixRef {
			continue
		}
		expired := compatible && !out[idx].ProtectedUntil.IsZero() && !r.StartedAt.Before(out[idx].ProtectedUntil)
		if !compatible || expired {
			if found {
				prev := &out[idx]
				prev.Current = false
				prev.ClosedAt = r.StartedAt
				if !prev.ProtectedUntil.IsZero() && prev.ProtectedUntil.Before(prev.ClosedAt) {
					prev.ClosedAt = prev.ProtectedUntil
				}
			}
			st := Stretch{ChatRef: r.Chat(), TTL: ttl, StartedAt: r.StartedAt, LastRequestAt: r.StartedAt, Current: true, LeftCensored: r.StartedAt.Before(q.Since)}
			if st.CallerID == "" || st.SessionID == "" || st.ContextEpoch == "" {
				st.UnknownWarmth = true
				st.Reasons = append(st.Reasons, "incomplete_chat_attribution")
			}
			out = append(out, st)
			idx = len(out) - 1
			current[id] = idx
		}
		st := &out[idx]
		if r.Kind == "primary" && r.Activity.PrefixRef != "" {
			st.PrefixRef = r.Activity.PrefixRef
		}
		st.Requests++
		st.LastRequestAt = r.StartedAt
		v := ValueRecord(r, e.Book)
		if !r.Final || r.EndedAt.After(q.AsOf) {
			st.InFlight = true
			v.Incomplete++
			st.Reasons = append(st.Reasons, "in_flight_attempt")
		}
		st.Value.add(v)
		if qualifies(r) {
			st.QualifyingRequests++
			if ttl.Duration() > 0 {
				until := r.StartedAt.Add(ttl.Duration())
				if until.After(st.ProtectedUntil) {
					st.ProtectedUntil = until
				}
			} else {
				st.UnknownWarmth = true
				st.Reasons = append(st.Reasons, "unknown_effective_ttl")
			}
			if r.Activity.Partial {
				st.UnknownWarmth = true
				st.Reasons = append(st.Reasons, "partial_prefix_coverage")
			}
		} else if r.Success && st.ProtectedUntil.IsZero() {
			// No proof that this successful primary refreshed all relevant cache.
			st.UnknownWarmth = true
			st.Reasons = append(st.Reasons, "unconfirmed_primary_prefix")
		}
	}
	for i := range out {
		st := &out[i]
		if st.Current && !st.UnknownWarmth && !st.InFlight && !st.ProtectedUntil.IsZero() && !q.AsOf.Before(st.ProtectedUntil) {
			st.ClosedAt = st.ProtectedUntil
		}
		through := q.AsOf
		if !st.ClosedAt.IsZero() && st.ClosedAt.Before(through) {
			through = st.ClosedAt
		}
		st.Coverage = coverage(e, st.AccountID, st.StartedAt, through)
		if !st.Coverage.Observed {
			st.Reasons = append(st.Reasons, st.Coverage.Reasons...)
		}
		if st.LeftCensored {
			st.Reasons = append(st.Reasons, "left_censored_stretch")
		}
		if !st.Value.Exact() {
			st.Reasons = append(st.Reasons, "unresolved_value")
		}
		if st.QualifyingRequests == 0 {
			st.Reasons = append(st.Reasons, "no_qualifying_activity")
		}
		st.Reasons = unique(st.Reasons)
		st.PriorEligible = len(st.Reasons) == 0 && st.QualifyingRequests >= 3 && !st.ClosedAt.IsZero() && !st.StartedAt.Before(q.AsOf.Add(-7*24*time.Hour))
	}
	return out
}

type Expectation struct {
	Known             bool    `json:"known"`
	USD               float64 `json:"usd"`
	LastUSD           float64 `json:"last_usd"`
	P75USD            float64 `json:"p75_usd"`
	PopulationSamples int     `json:"population_samples"`
	HasLast           bool    `json:"has_last"`
	LastKnown         bool    `json:"last_known"`
	Confidence        string  `json:"confidence"`
	TTL               TTL     `json:"ttl"`
}

// Expected uses the last completed stretch in the same account/context and the
// exact provider/TTL-compatible seven-day population. An unknown last stretch
// cannot be skipped in favor of cheaper older history. p75 is nearest-rank.
func (r Report) Expected(chat ChatRef, ttl TTL) Expectation {
	x := Expectation{TTL: ttl, Confidence: "unknown"}
	if ttl.Duration() == 0 {
		return x
	}
	var values []float64
	var latest time.Time
	for _, s := range r.Stretches {
		if s.Provider != chat.Provider || s.TTL != ttl {
			continue
		}
		if s.PriorEligible {
			values = append(values, s.Value.LowUSD())
		}
		if s.ChatRef == chat && !s.ClosedAt.IsZero() && s.StartedAt.After(latest) {
			x.HasLast = true
			x.LastKnown = len(s.Reasons) == 0
			x.LastUSD = 0
			if x.LastKnown {
				x.LastUSD = s.Value.LowUSD()
			}
			latest = s.StartedAt
		}
	}
	x.PopulationSamples = len(values)
	if len(values) > 0 {
		sort.Float64s(values)
		x.P75USD = values[int(math.Ceil(.75*float64(len(values))))-1]
	}
	if x.HasLast && !x.LastKnown {
		x.Confidence = "unknown_last_stretch"
		return x
	}
	x.Known = x.HasLast || len(values) > 0
	if x.Known {
		x.USD = math.Max(x.LastUSD, x.P75USD)
		x.Confidence = "sparse"
		if len(values) >= 4 {
			x.Confidence = "measured"
		}
	}
	return x
}

type Burn struct {
	Known          bool     `json:"known"`
	Bounded        bool     `json:"bounded"`
	LowUSDPerHour  float64  `json:"low_usd_per_hour"`
	HighUSDPerHour float64  `json:"high_usd_per_hour"`
	WarmChats      int      `json:"warm_chats"`
	UnknownChats   int      `json:"unknown_chats"`
	Reasons        []string `json:"reasons,omitempty"`
}

func accountBurn(stretches []Stretch, account string, at time.Time) Burn {
	b := Burn{Known: true, Bounded: true}
	for _, s := range stretches {
		if s.AccountID != account || !s.Warm(at) {
			continue
		}
		b.WarmChats++
		hours := at.Sub(s.StartedAt).Hours()
		if hours <= 0 || s.UnknownWarmth || s.InFlight || s.LeftCensored || !s.Coverage.Observed || !s.Value.Bounded() {
			b.UnknownChats++
			b.Known = false
			b.Bounded = false
			b.Reasons = append(b.Reasons, "unknown_warm_burn")
			continue
		}
		b.LowUSDPerHour += s.Value.LowUSD() / hours
		b.HighUSDPerHour += s.Value.HighUSD() / hours
		if !s.Value.Exact() {
			b.Known = false
			b.Reasons = append(b.Reasons, "bounded_write_mix_burn")
		}
	}
	b.Reasons = unique(b.Reasons)
	return b
}

type Runway struct {
	Known     bool    `json:"known"`
	Hours     float64 `json:"hours"`
	BlocksNew bool    `json:"blocks_new"`
	Reason    string  `json:"reason"`
}

// Runway uses the upper *bounded* burn estimate. Unknown burn does not mean zero
// load: BlocksNew conservatively withholds new placement; it never evicts a chat.
// The caller supplies a fresh, source-backed budget/reset and a measured ratio.
func (b Burn) Runway(availableUSD float64, asOf, reset time.Time) Runway {
	if !finite(availableUSD) || availableUSD < 0 || !validTime(asOf) || !validTime(reset) || !reset.After(asOf) {
		return Runway{BlocksNew: true, Reason: "unknown_budget_or_reset"}
	}
	if !b.Bounded || !finite(b.HighUSDPerHour) || b.HighUSDPerHour < 0 {
		return Runway{BlocksNew: true, Reason: "unknown_warm_burn"}
	}
	if b.HighUSDPerHour == 0 {
		return Runway{Known: true, Reason: "no_observed_warm_burn"}
	}
	hours := availableUSD / b.HighUSDPerHour
	return Runway{Known: true, Hours: hours, BlocksNew: hours < reset.Sub(asOf).Hours(), Reason: "upper_bounded_warm_burn"}
}
