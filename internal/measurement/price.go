package measurement

import (
	"fmt"
	"math"
	"sort"
)

// Rates are USD per million tokens. Nil is unpriced; a pointer to zero is an
// explicitly free category. There are no guessed multipliers or model aliases.
type Rates struct {
	Input   *float64 `json:"input,omitempty"`
	Read    *float64 `json:"read,omitempty"`
	Write5m *float64 `json:"write_5m,omitempty"`
	Write1h *float64 `json:"write_1h,omitempty"`
	Output  *float64 `json:"output,omitempty"`
}

// Price applies to [MinInput,MaxInput), where MaxInput=0 is unbounded. Explicit
// tier/context entries have full rates, not implicit family-wide multipliers.
type Price struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	ServiceTier string `json:"service_tier"`
	MinInput    int64  `json:"min_input"`
	MaxInput    int64  `json:"max_input"`
	Rates       Rates  `json:"rates"`
	Source      string `json:"source"`
}

type PriceBook struct {
	ID      string  `json:"id"`
	Version string  `json:"version"`
	Prices  []Price `json:"prices"`
}

func number(n float64) *float64 { return &n }

// DefaultPriceBook pins only independently authorized rates. Other local model
// catalogues are not authoritative price books; see README.md's offline audit.
func DefaultPriceBook() PriceBook {
	return PriceBook{
		ID: "pacer-prices-2026-10-08-opus55-v1", Version: "2026-10-08.1",
		Prices: []Price{{Provider: "claude", Model: "claude-opus-5-5", ServiceTier: "standard",
			Rates:  Rates{number(4), number(.2), number(5), number(8), number(20)},
			Source: "Pacer PLAN.md section 3.1 / CACHE-TTL.md section 2; user-pinned 2026-10-08; https://platform.claude.com/docs/en/about-claude/pricing"}},
	}
}

func standardTier(t string) string {
	if t == "" || t == "default" || t == "standard" {
		return "standard"
	}
	return t // "auto" is a selection request, not proof of an actual charged tier.
}

func validateBook(b PriceBook) error {
	if b.ID == "" || b.Version == "" || len(b.Prices) == 0 {
		return ErrInvalid
	}
	for i, p := range b.Prices {
		if p.Provider == "" || p.Model == "" || p.Source == "" || p.ServiceTier == "" || p.MinInput < 0 || p.MaxInput != 0 && p.MaxInput <= p.MinInput {
			return ErrInvalid
		}
		for _, r := range []*float64{p.Rates.Input, p.Rates.Read, p.Rates.Write5m, p.Rates.Write1h, p.Rates.Output} {
			if r != nil && (!finite(*r) || *r < 0) {
				return ErrInvalid
			}
		}
		for _, q := range b.Prices[:i] {
			if p.Provider == q.Provider && p.Model == q.Model && standardTier(p.ServiceTier) == standardTier(q.ServiceTier) && (q.MaxInput == 0 || p.MinInput < q.MaxInput) && (p.MaxInput == 0 || q.MinInput < p.MaxInput) {
				return fmt.Errorf("%w: overlapping price tiers", ErrInvalid)
			}
		}
	}
	return nil
}

type Value struct {
	LowNanoUSD      int64    `json:"low_nano_usd"`
	HighNanoUSD     int64    `json:"high_nano_usd"`
	ObservedWrite5m int64    `json:"observed_write_5m"`
	ObservedWrite1h int64    `json:"observed_write_1h"`
	InferredWrites  int64    `json:"inferred_writes"`
	UnknownWrites   int64    `json:"unknown_writes"`
	MissingPrices   int      `json:"missing_price_records"`
	InvalidCounters int      `json:"invalid_counter_records"`
	Incomplete      int      `json:"incomplete_records"`
	Issues          []string `json:"issues,omitempty"`
}

func (v Value) Bounded() bool {
	return v.MissingPrices == 0 && v.InvalidCounters == 0 && v.Incomplete == 0
}
func (v Value) Exact() bool      { return v.Bounded() && v.LowNanoUSD == v.HighNanoUSD }
func (v Value) LowUSD() float64  { return float64(v.LowNanoUSD) / 1e9 }
func (v Value) HighUSD() float64 { return float64(v.HighNanoUSD) / 1e9 }

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }
func counter(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

func sumChecked(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

func normalized(u Usage) (input int64, valid bool) {
	if u.NonCacheInput < 0 || u.CacheRead < 0 || u.CacheCreate < 0 || counter(u.CacheCreate5m) < 0 || counter(u.CacheCreate1h) < 0 || u.Output < 0 || u.Reasoning < 0 || u.Reasoning > u.Output {
		return 0, false
	}
	writes, ok := sumChecked(counter(u.CacheCreate5m), counter(u.CacheCreate1h))
	if !ok || writes > u.CacheCreate {
		return 0, false
	}
	input, ok = sumChecked(u.NonCacheInput, u.CacheRead)
	if !ok {
		return 0, false
	}
	input, ok = sumChecked(input, u.CacheCreate)
	if !ok || u.TotalInput != nil && *u.TotalInput != input {
		return 0, false
	}
	total, ok := sumChecked(input, u.Output)
	return input, ok && (u.Total == nil || *u.Total == total)
}

// ValueRecord preserves observed splits, only infers a remainder with complete
// executor coverage, and prices any other remainder as a range. It is pure.
func ValueRecord(r Record, b PriceBook) Value {
	u := r.Usage
	v := Value{ObservedWrite5m: counter(u.CacheCreate5m), ObservedWrite1h: counter(u.CacheCreate1h)}
	input, valid := normalized(u)
	if !valid {
		v.InvalidCounters = 1
		v.Issues = []string{"invalid_counters"}
		return v
	}
	v.UnknownWrites = u.CacheCreate - v.ObservedWrite5m - v.ObservedWrite1h
	if r.Policy.SelectedTTL != "" && r.Policy.EffectiveTTL != "" && r.Policy.SelectedTTL != r.Policy.EffectiveTTL {
		v.Issues = append(v.Issues, "selected_wire_mismatch")
	}
	wireMismatch := r.Policy.AllWritesCovered && (r.Policy.EffectiveTTL == TTL5m && v.ObservedWrite1h > 0 || r.Policy.EffectiveTTL == TTL1h && v.ObservedWrite5m > 0)
	if wireMismatch {
		v.Issues = append(v.Issues, "wire_usage_mismatch")
	}
	if u.AccountingVersion == "" || u.Quality != "complete" || !r.Final {
		v.Incomplete = 1
		v.Issues = append(v.Issues, "incomplete_accounting")
	}
	if input == 0 && u.Output == 0 {
		return v
	}
	var rates Rates
	found := false
	for _, p := range b.Prices {
		if p.Provider == r.Attribution.Provider && p.Model == r.Attribution.Model && standardTier(p.ServiceTier) == standardTier(r.Attribution.ServiceTier) && input >= p.MinInput && (p.MaxInput == 0 || input < p.MaxInput) {
			if found {
				v.MissingPrices = 1
				v.Issues = append(v.Issues, "ambiguous_price")
				return v
			}
			rates, found = p.Rates, true
		}
	}
	if !found {
		v.MissingPrices = 1
		v.Issues = append(v.Issues, "missing_model_or_tier_price")
		return v
	}
	missing := false
	charge := func(tokens int64, rate *float64) int64 {
		if tokens == 0 {
			return 0
		}
		if rate == nil || !finite(*rate) || *rate < 0 {
			missing = true
			return 0
		}
		n := float64(tokens) * (*rate) * 1000
		// Keep individual categories and their sum well inside the signed range.
		if !finite(n) || n >= float64(math.MaxInt64)/16 {
			missing = true
			return 0
		}
		return int64(math.Round(n))
	}
	known := charge(u.NonCacheInput, rates.Input) + charge(u.CacheRead, rates.Read) + charge(u.Output, rates.Output) + charge(v.ObservedWrite5m, rates.Write5m) + charge(v.ObservedWrite1h, rates.Write1h)
	v.LowNanoUSD, v.HighNanoUSD = known, known
	if v.UnknownWrites > 0 {
		var inferred *float64
		if r.Policy.AllWritesCovered {
			switch r.Policy.EffectiveTTL {
			case TTL5m:
				inferred = rates.Write5m
			case TTL1h:
				inferred = rates.Write1h
			}
			// Do not infer from internally contradictory all-writes policy evidence.
			if wireMismatch {
				inferred = nil
			}
		}
		if inferred != nil {
			n := charge(v.UnknownWrites, inferred)
			v.LowNanoUSD += n
			v.HighNanoUSD += n
			v.InferredWrites, v.UnknownWrites = v.UnknownWrites, 0
			v.Issues = append(v.Issues, "policy_inferred_writes")
		} else {
			lo, hi := charge(v.UnknownWrites, rates.Write5m), charge(v.UnknownWrites, rates.Write1h)
			v.LowNanoUSD += min(lo, hi)
			v.HighNanoUSD += max(lo, hi)
			v.Issues = append(v.Issues, "unclassified_cache_writes")
		}
	}
	if missing {
		v.MissingPrices = 1
		v.Issues = append(v.Issues, "missing_category_price_or_overflow")
	}
	return v
}

func (v *Value) add(w Value) {
	for _, pair := range [][2]*int64{{&v.LowNanoUSD, &w.LowNanoUSD}, {&v.HighNanoUSD, &w.HighNanoUSD}, {&v.ObservedWrite5m, &w.ObservedWrite5m}, {&v.ObservedWrite1h, &w.ObservedWrite1h}, {&v.InferredWrites, &w.InferredWrites}, {&v.UnknownWrites, &w.UnknownWrites}} {
		n, ok := sumChecked(*pair[0], *pair[1])
		if !ok {
			v.InvalidCounters++
			v.Issues = append(v.Issues, "aggregate_overflow")
		} else {
			*pair[0] = n
		}
	}
	v.MissingPrices += w.MissingPrices
	v.InvalidCounters += w.InvalidCounters
	v.Incomplete += w.Incomplete
	v.Issues = unique(append(v.Issues, w.Issues...))
}

func unique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	sort.Strings(s)
	out := s[:0]
	for _, x := range s {
		if len(out) == 0 || out[len(out)-1] != x {
			out = append(out, x)
		}
	}
	return out
}
