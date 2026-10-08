package capacity

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resetTolerance matches the observer's cycle tolerance for the same reset.
const resetTolerance = 2 * time.Second

type Value struct {
	LowerBoundUSDPerPercent float64 `json:"lower_bound_usd_per_percent"`
	CPAValueUSD             float64 `json:"cpa_value_usd"`
	DeltaPercent            float64 `json:"delta_percent"`
	QuantumPercent          float64 `json:"quantum_percent"`
	Records                 int     `json:"records"`
}

// Interval is one judged pair of samples, dated by its own interval.
type Interval struct {
	Status      string    `json:"status"`
	Reasons     []string  `json:"reasons"`
	Exclusivity string    `json:"exclusivity"`
	Window      string    `json:"window"`
	From        time.Time `json:"interval_from"`
	To          time.Time `json:"interval_to"`
	Value       *Value    `json:"value"`
	Basis       string    `json:"basis"`
	seconds     int64
}

// Quantum is the provider's reported precision of a percent value: 1 for an
// integer and 10^-d for d significant decimals. Trailing zeros do not claim
// precision. Unparseable or out-of-range values have unknown precision (0).
func Quantum(raw string) float64 {
	raw = strings.TrimSpace(raw)
	whole, frac, dotted := strings.Cut(raw, ".")
	if !digits(whole) || dotted && !digits(frac) {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 100 {
		return 0
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > 9 {
		return 0
	}
	return math.Pow10(-len(frac))
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Project is CPA value(A, interval) / (Δ% + quantum). It is unknown unless M and
// K hold, every counted record is priced and complete, both samples are of one
// window and cycle, both precisions are known and Δ% + quantum > 0.
func Project(s1, s2 Sample, m Membership, k CarryIn) Interval {
	i := Interval{Status: "unknown", Reasons: []string{}, Exclusivity: "unproven", Window: s2.Window.Key(), From: s1.At, To: s2.At, Basis: Basis, seconds: s2.Window.Seconds}
	add := func(r ...string) { i.Reasons = append(i.Reasons, r...) }
	if !m.Holds {
		add("membership_unproven")
		add(m.Reasons...)
	}
	if !k.Holds {
		add("carry_in_unproven")
		add(k.Reasons...)
	}
	if s1.Account != s2.Account {
		add("account_mismatch")
	}
	if s1.Window != s2.Window {
		add("windows_differ")
	}
	switch {
	case s1.Reset.IsZero() || s2.Reset.IsZero():
		add("reset_unknown")
	case s1.Reset.Sub(s2.Reset).Abs() > resetTolerance || !s1.Reset.After(s2.At):
		add("reset_between_samples")
	}
	quantum := math.Max(s1.Quantum, s2.Quantum)
	delta := s2.UsedPercent - s1.UsedPercent
	if !(s1.Quantum > 0) || !(s2.Quantum > 0) || !finite(quantum) {
		add("precision_unknown")
	} else if !finite(delta) || delta+quantum <= 0 {
		add("nonpositive_change")
	}
	var nano int64
	for _, r := range k.Records {
		if !r.Priced {
			add("record_unpriced")
		}
		if !r.Delivered || !r.Final || !r.CounterComplete {
			add("record_incomplete")
		}
		if r.LowNanoUSD < 0 || nano > math.MaxInt64-r.LowNanoUSD {
			add("value_overflow")
		} else {
			nano += r.LowNanoUSD
		}
	}
	if i.Reasons = unique(i.Reasons); len(i.Reasons) > 0 {
		return i
	}
	i.Reasons = []string{}
	usd := float64(nano) / 1e9
	i.Status = "lower_bound"
	i.Value = &Value{LowerBoundUSDPerPercent: usd / (delta + quantum), CPAValueUSD: usd, DeltaPercent: delta, QuantumPercent: quantum, Records: len(k.Records)}
	return i
}

// Pairs returns consecutive samples of each account window as (older, newer),
// at most perWindow per window, newest first. Input is newest first.
func Pairs(samples []Sample, perWindow int) [][2]Sample {
	groups := map[string][]Sample{}
	for _, s := range samples {
		key := s.Account + "\x00" + s.Window.Key()
		groups[key] = append(groups[key], s)
	}
	var out [][2]Sample
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool { return g[i].At.After(g[j].At) })
		for i := 0; i+1 < len(g) && i < perWindow; i++ {
			out = append(out, [2]Sample{g[i+1], g[i]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i][1].At.Equal(out[j][1].At) {
			return out[i][1].At.After(out[j][1].At)
		}
		return out[i][1].Window.Seconds > out[j][1].Window.Seconds
	})
	return out
}
