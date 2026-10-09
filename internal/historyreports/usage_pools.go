package historyreports

// Quota pools of the usage record. Providers and recorders named the same meter
// differently over time (CPAMP's response headers, CPAMP's own API polls and
// Pacer's parser). Every reading is mapped to one canonical pool before windows
// are derived, so a meter has one window list whatever recorded it.
//
//	Anthropic   all (5h, weekly); fable (weekly 7d_oi: Fable models, Max plans)
//	Codex       codex-main (weekly; 5h ended); Spark (5h/weekly, ended 2026-09);
//	            one window per cycle, ending where the next cycle begins
//	Antigravity gemini and claude_gpt (each 5h and weekly)
//	OpenCode Go all (5h rolling, weekly, monthly)

import (
	"sort"
	"strconv"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// PoolsVersion changes whenever the mapping below changes; it is part of every
// sealed record key, so a new mapping re-derives the affected windows.
const PoolsVersion = "pools-v2" // v2: Codex twins joined, early ends closed (codexCycles)

const (
	sparkScope = "model:gpt-5.3-codex-spark"
	fableScope = "fable"
)

// poolMapping is switched off only by tests that compare against CPAMP's own
// pool names.
var poolMapping = true

// canonicalScope maps a provider's raw scope name to its pool.
func canonicalScope(provider, scope string) string {
	if !poolMapping {
		return scope
	}
	s := strings.ToLower(strings.TrimSpace(scope))
	switch provider {
	case "claude":
		switch s {
		case "unknown:7d_oi", "oi", "7d_oi", "limit:oi", fableScope:
			return fableScope
		}
	case "codex":
		switch s {
		case "all", "codex-main", "codex_main", "codex":
			return "codex-main"
		case "spark", "codex_bengalfox", "bengalfox", sparkScope:
			return sparkScope
		}
	}
	return scope
}

// fableModel reports whether a model counts against Anthropic's Fable limit.
func fableModel(model string) bool { return strings.Contains(strings.ToLower(model), "fable") }

// canonicalObservations rewrites readings to their pools in place. Window IDs
// are kept: an imported window keeps its identity (and its frozen valuation
// cohort) whether or not its scope name changed. The Fable pool is a model
// list (the Fable models in the given events), because the extracted matcher
// knows only CPAMP's scope names. Codex cycles are then made one window each,
// ending where the next one begins (codexCycles).
func canonicalObservations(obs []cpamp.Observation, events []cpamp.Event) {
	var fable []string
	for i := range obs {
		o := &obs[i]
		scope := canonicalScope(o.Provider, o.Scope)
		if scope == o.Scope && scope != fableScope {
			continue
		}
		o.Scope = scope
		switch scope {
		case fableScope:
			if fable == nil {
				seen := map[string]bool{}
				fable = []string{}
				for _, e := range events {
					if fableModel(e.Model) && !seen[e.Model] {
						seen[e.Model] = true
						fable = append(fable, e.Model)
					}
				}
			}
			o.Models = fable
		case sparkScope:
			o.Models = []string{"gpt-5.3-codex-spark"}
		default:
			o.Models = nil
		}
	}
	if poolMapping {
		codexCycles(obs)
	}
}

// boundaryJitterMS is how far apart two readings may put one Codex window's
// boundaries. Codex reports a reset as seconds from now, so response headers
// and API polls (and both recorders) disagree by a few seconds: 3–4 s on real
// data, where Derive joins readings only within 2 s. Distinct windows are
// hours apart.
const boundaryJitterMS = 10000

// codexCycles makes every Codex cycle one window with its real span, in two
// steps, before Derive reads the readings.
//
// Twins. Derive would make a jittered twin a second window of the same meter:
// listed twice, and the real window's measurement restarts at every twin
// reading (one real week measured 11 of its 95 points). Twins read the same
// percentage at the same moments. Readings are grouped as Derive groups them
// (time order; start and reset within 2 s of a group's first reading). In each
// pool the group with the most readings takes every other group whose first
// reading's start and reset are within boundaryJitterMS of its own: those
// readings get its start, reset and window ID, so Derive puts them in its
// window and its ID stays.
//
// Early ends. Codex often ends a cycle early. Derive notices that only through
// the next window's first reading, and skips that reading when it looks like
// an idle forecast (0% at the window's start), which is what a new cycle's
// first reading usually is. The earlier window then keeps its scheduled reset
// and its requests overlap the next window's for days. When a later cycle
// starts inside a window's scheduled cycle and after its last reading, the
// window's cycle ended by then: Pacer attaches closure evidence to the later
// cycle's first reading (cpamp.CycleClosure: End = the later cycle's start,
// Earliest = the window's last reading), the source-backed early end CPAMP's
// v3 derivation accepts. The window then ends where the next one begins and
// is "closed"; its readings and rate are unchanged. A window whose readings
// continue past a later window's start (two meters reported at once) gets no
// closure.
//
// Idle forecasts (every reading 0% and the start forecast at the reading's own
// time) take part in neither step; Derive drops them once a later reading
// exists.
func codexCycles(obs []cpamp.Observation) {
	order := make([]int, 0, len(obs))
	closed := map[string]bool{} // windows that already carry closure evidence
	for i := range obs {
		o := &obs[i]
		if o.ClosedCycle != nil {
			closed[o.ClosedCycle.WindowID] = true
		}
		if o.Provider == "codex" && o.Start > 0 && o.Reset > o.Start {
			order = append(order, i)
		}
	}
	if len(order) < 2 {
		return
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := &obs[order[a]], &obs[order[b]]
		if x.Observed != y.Observed {
			return x.Observed < y.Observed
		}
		return x.ID < y.ID
	})
	type group struct {
		first   *cpamp.Observation
		members []*cpamp.Observation // time order
		into    *group
		last    int64 // newest reading, twins included
	}
	pools := map[string][]*group{}
	index := map[string][]*group{} // kind|scope|reset bucket, as Derive indexes
	for _, i := range order {
		o := &obs[i]
		key := o.Kind + "|" + o.Scope
		var g *group
		for b := o.Reset/2001 - 1; b <= o.Reset/2001+1 && g == nil; b++ {
			for _, c := range index[key+"|"+strconv.FormatInt(b, 10)] {
				if abs64(c.first.Reset-o.Reset) <= 2000 && abs64(c.first.Start-o.Start) <= 2000 {
					g = c
					break
				}
			}
		}
		if g == nil {
			g = &group{first: o}
			pools[key] = append(pools[key], g)
			bucket := key + "|" + strconv.FormatInt(o.Reset/2001, 10)
			index[bucket] = append(index[bucket], g)
		}
		g.members = append(g.members, o)
	}
	for _, groups := range pools {
		live := make([]*group, 0, len(groups))
		for _, g := range groups {
			idle := abs64(g.first.Start-g.first.Observed) <= 10000
			for _, o := range g.members {
				if o.Used == nil || *o.Used != 0 {
					idle = false
					break
				}
			}
			if !idle {
				live = append(live, g)
			}
		}
		if len(live) < 2 {
			continue
		}
		byStart := append([]*group(nil), live...)
		sort.SliceStable(byStart, func(a, b int) bool { return byStart[a].first.Start < byStart[b].first.Start })
		ranked := append([]*group(nil), live...) // most readings first; ties keep the earlier window
		sort.SliceStable(ranked, func(a, b int) bool { return len(ranked[a].members) > len(ranked[b].members) })
		for _, g := range ranked {
			if g.into != nil {
				continue
			}
			from := sort.Search(len(byStart), func(i int) bool { return byStart[i].first.Start >= g.first.Start-boundaryJitterMS })
			for _, h := range byStart[from:] {
				if h.first.Start > g.first.Start+boundaryJitterMS {
					break
				}
				// A larger group in range was handled first (and took g), so
				// h here never has twins of its own.
				if h != g && h.into == nil && abs64(h.first.Reset-g.first.Reset) <= boundaryJitterMS {
					h.into = g
				}
			}
		}
		cycles := []*group{}
		for _, g := range byStart {
			if g.into == nil {
				cycles = append(cycles, g)
			}
		}
		for _, g := range byStart {
			root := g
			if g.into != nil {
				root = g.into
				for _, o := range g.members {
					o.Start, o.Reset, o.Duration, o.WindowID = root.first.Start, root.first.Reset, root.first.Duration, root.first.WindowID
				}
				root.members = append(root.members, g.members...)
			}
			for _, o := range g.members {
				root.last = max(root.last, o.Observed)
			}
		}
		for _, g := range cycles {
			sort.SliceStable(g.members, func(a, b int) bool { return g.members[a].Observed < g.members[b].Observed })
		}
		for i, g := range cycles {
			if closed[g.first.WindowID] || g.last <= g.first.Start {
				continue
			}
			for _, n := range cycles[i+1:] {
				if n.first.Start <= g.last || n.first.Start <= g.first.Start+boundaryJitterMS {
					continue
				}
				if n.first.Start >= g.first.Reset {
					break // the next cycle began after this one's reset
				}
				for _, o := range n.members {
					if o.Observed >= n.first.Start && o.ClosedCycle == nil && o.Source != "" {
						o.ClosedCycle = &cpamp.CycleClosure{WindowID: g.first.WindowID, End: n.first.Start, Earliest: g.last, Latest: n.first.Start, Status: "closed"}
						closed[g.first.WindowID] = true
						break
					}
				}
				break
			}
		}
	}
}

// PoolLabel names a pool for people. Kind names are added by the page.
func PoolLabel(provider, scope string) string {
	switch scope {
	case "all", "codex-main", "":
		return ""
	case fableScope:
		return "Fable"
	case sparkScope:
		return "Spark"
	case "gemini":
		return "Gemini models"
	case "claude_gpt":
		return "Claude & GPT models"
	}
	if strings.HasPrefix(scope, "model:") {
		return strings.TrimPrefix(scope, "model:")
	}
	if strings.HasPrefix(scope, "unknown") {
		return "Unverified limit"
	}
	return scope
}

func mainScope(scope string) bool {
	return scope == "all" || scope == "codex-main" || scope == "gemini"
}

// kindOrder sorts columns: month, week, 5 hours.
func kindOrder(kind string) int {
	switch kind {
	case "monthly":
		return 0
	case "weekly":
		return 1
	case "5h":
		return 2
	}
	return 3
}

const dayMS = int64(86400000)

// kindDuration is the longest provider window of a kind in milliseconds.
func kindDuration(kind string) int64 {
	switch kind {
	case "5h":
		return 5 * 3600000
	case "weekly":
		return 7 * dayMS
	case "monthly":
		return 31 * dayMS
	}
	return 0
}

func validKind(kind string) bool { return cpamp.FactKind(kind) }
