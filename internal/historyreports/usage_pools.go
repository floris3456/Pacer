package historyreports

// Quota pools of the usage record. Providers and recorders named the same meter
// differently over time (CPAMP's response headers, CPAMP's own API polls and
// Pacer's parser). Every reading is mapped to one canonical pool before windows
// are derived, so a meter has one window list whatever recorded it.
//
//	Anthropic   all (5h, weekly); fable (weekly 7d_oi: Fable models, Max plans)
//	Codex       codex-main (weekly; 5h ended); Spark (5h/weekly, ended 2026-09)
//	Antigravity gemini and claude_gpt (each 5h and weekly)
//	OpenCode Go all (5h rolling, weekly, monthly)

import (
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// PoolsVersion changes whenever the mapping below changes; it is part of every
// sealed record key, so a new mapping re-derives the affected windows.
const PoolsVersion = "pools-v1"

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
// knows only CPAMP's scope names.
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
