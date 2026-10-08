package dryrun

import (
	"math"
	"sort"
	"time"
)

// Assumptions every known projection makes; docs/dry-run-report.md explains them.
var assumptions = []string{
	"equal_quality",                         // a moved request has the same API-equivalent value on its target account
	"proposals_conditioned_on_cpa_bindings", // dry-run proposals saw CPA's bindings, not the simulated ones
	"no_floor_feedback",                     // added load does not trigger further simulated moves
}

type group struct {
	served    string
	ambiguous bool
}

type attemptSum struct {
	nano     int64
	exact    bool
	count    int
	open     bool
	overflow bool
	first    time.Time
	last     time.Time
}

func joinKey(request, account string) string { return request + "\x00" + account }

// index builds the decision and usage joins once, on first use.
func (b *builder) index() {
	if b.groups != nil {
		return
	}
	b.groups = map[string]group{}
	for i, d := range b.ds {
		if d.Outcome != "admitted" || d.RequestID == "" || d.ActualAccount == "" {
			continue
		}
		k := joinKey(d.RequestID, d.ActualAccount)
		g, ok := b.groups[k]
		if !ok {
			b.groups[k] = group{served: b.served[i]}
			continue
		}
		if g.served != b.served[i] {
			g.ambiguous = true
			b.groups[k] = g
		}
	}
	b.attempts = map[string]*attemptSum{}
	for _, a := range b.in.Attempts {
		k := joinKey(a.RequestID, a.Account)
		if _, wanted := b.groups[k]; !wanted {
			continue
		}
		s := b.attempts[k]
		if s == nil {
			s = &attemptSum{exact: true, first: a.StartedAt, last: a.EndedAt}
			b.attempts[k] = s
		}
		s.count++
		s.exact = s.exact && a.Exact
		if a.NanoUSD < 0 || s.nano > math.MaxInt64-a.NanoUSD {
			s.overflow = true
		} else {
			s.nano += a.NanoUSD
		}
		s.open = s.open || a.EndedAt.IsZero()
		s.first = earlier(s.first, a.StartedAt)
		s.last = later(s.last, a.EndedAt)
	}
}

func (b *builder) resets() []Reset {
	out := []Reset{}
	for _, c := range b.in.Cycles {
		var at time.Time
		var basis string
		switch {
		case c.Closed.IsZero() && c.Scheduled.IsZero():
			continue
		case c.Scheduled.IsZero():
			at, basis = c.Closed, "observed"
		case c.Closed.IsZero():
			at, basis = c.Scheduled, "scheduled_unconfirmed"
		case c.Closed.Before(c.Scheduled):
			at, basis = c.Closed, "observed_early"
		default:
			at, basis = c.Scheduled, "scheduled"
		}
		at = at.UTC()
		if !b.inDay(at) || at.After(b.in.Now) {
			continue
		}
		r := Reset{Provider: c.Provider, Account: c.Account, Scope: c.Scope, WindowSeconds: c.Seconds, CycleID: c.CycleID, ResetAt: at, ResetBasis: basis}
		r.Baseline = baseline(c, at)
		if !c.Scheduled.IsZero() && c.Seconds > 0 {
			start := c.Scheduled.Add(-time.Duration(c.Seconds) * time.Second).UTC()
			r.CycleStart = &start
		}
		r.Projection = b.project(c, r)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, z := out[i], out[j]
		switch {
		case !a.ResetAt.Equal(z.ResetAt):
			return a.ResetAt.Before(z.ResetAt)
		case a.Provider != z.Provider:
			return a.Provider < z.Provider
		case a.Account != z.Account:
			return a.Account < z.Account
		case a.Scope != z.Scope:
			return a.Scope < z.Scope
		case a.WindowSeconds != z.WindowSeconds:
			return a.WindowSeconds < z.WindowSeconds
		}
		return a.CycleID < z.CycleID
	})
	return out
}

// baseline is the last usable reading before the reset. Usage only grows within
// a cycle, so a stale reading is an upper bound on the unused percent.
func baseline(c Cycle, at time.Time) Baseline {
	b := Baseline{Policy: "fill_first_observed", Status: "none"}
	if c.Last == nil || math.IsNaN(c.Last.UsedPercent) || c.Last.UsedPercent < 0 || c.Last.UsedPercent > 100 {
		return b
	}
	unused := 100 - c.Last.UsedPercent
	observed := c.Last.At.UTC()
	gap := int64(0)
	if at.After(observed) {
		gap = int64(at.Sub(observed) / time.Second)
	}
	b.UnusedPercent, b.ObservedAt, b.GapSeconds = &unused, &observed, &gap
	b.Status = "fresh"
	if time.Duration(gap)*time.Second > FreshWithin {
		b.Status = "stale"
	}
	return b
}

// project computes projected_unused = observed_unused − r·(value_in − value_out)
// for the cycle's account, where r is the cycle's own measured percent per
// dollar. Any missing or doubtful input leaves the projection unknown.
func (b *builder) project(c Cycle, r Reset) Projection {
	p := Projection{Policy: "pace_dry_run_simulation", Assumptions: append([]string(nil), assumptions...), Reasons: []string{}}
	reasons := []string{}
	if r.Baseline.Status != "fresh" {
		reasons = append(reasons, "baseline_"+r.Baseline.Status)
	}
	ratio := c.Ratio.USDPerPercent
	if !c.Ratio.Known || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		reasons = append(reasons, "ratio_unknown")
		for _, reason := range c.Ratio.Reasons {
			reasons = append(reasons, "ratio:"+reason)
		}
	} else {
		perUSD := 1 / ratio
		p.PercentPerUSD = &perUSD
	}
	if b.in.AttemptsUnavailable != "" {
		reasons = append(reasons, b.in.AttemptsUnavailable)
	}
	var in, out int64
	if r.CycleStart == nil {
		reasons = append(reasons, "cycle_start_unknown")
	} else {
		start, end := *r.CycleStart, r.ResetAt
		if !b.in.Since.IsZero() && start.Before(b.in.Since) {
			reasons = append(reasons, "cycle_before_decision_window")
		}
		if !b.cov.contains(c.Provider, start, end) {
			reasons = append(reasons, "dry_run_coverage_incomplete")
		}
		if overlaps(b.in.ErrorSegments, start, end) {
			reasons = append(reasons, "decision_write_errors")
		}
		var moved []string
		in, out, p.RequestsIn, p.RequestsOut, moved = b.moved(c, start, end)
		reasons = append(reasons, moved...)
	}
	p.ValueInUSD, p.ValueOutUSD = float64(in)/1e9, float64(out)/1e9
	p.Reasons = unique(reasons)
	if len(p.Reasons) > 0 {
		return p
	}
	raw := *r.Baseline.UnusedPercent - *p.PercentPerUSD*float64(in-out)/1e9
	clamped := math.Min(100, math.Max(0, raw))
	p.Known, p.RawUnusedPercent, p.UnusedPercent = true, &raw, &clamped
	return p
}

// moved sums the priced usage of admitted requests in [start,end) that the
// simulation serves on a different account than CPA did, where either side is
// the cycle's account.
func (b *builder) moved(c Cycle, start, end time.Time) (in, out int64, inN, outN int, reasons []string) {
	b.index()
	seen := map[string]bool{}
	lo := sort.Search(len(b.ds), func(i int) bool { return !b.ds[i].At.Before(start) })
	for i := lo; i < len(b.ds) && b.ds[i].At.Before(end); i++ {
		d := b.ds[i]
		if d.Provider != c.Provider || d.Outcome != "admitted" {
			continue
		}
		if d.ActualAccount == "" {
			reasons = append(reasons, "decision_unattributed")
			continue
		}
		served := b.served[i]
		if served == d.ActualAccount || served != c.Account && d.ActualAccount != c.Account {
			continue
		}
		if d.RequestID == "" {
			reasons = append(reasons, "decision_without_request_id")
			continue
		}
		k := joinKey(d.RequestID, d.ActualAccount)
		if b.groups[k].ambiguous {
			reasons = append(reasons, "ambiguous_join")
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		a := b.attempts[k]
		if a == nil || a.count == 0 {
			reasons = append(reasons, "moved_request_without_usage")
			continue
		}
		if !a.exact || a.overflow {
			reasons = append(reasons, "unresolved_value")
		}
		if a.open || a.first.Before(start) || a.last.After(end) {
			reasons = append(reasons, "moved_request_crosses_cycle_boundary")
		}
		if d.ActualAccount == c.Account {
			out += a.nano
			outN++
		} else {
			in += a.nano
			inN++
		}
	}
	return
}

func unique(values []string) []string {
	sort.Strings(values)
	out := []string{}
	for _, v := range values {
		if len(out) == 0 || v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
