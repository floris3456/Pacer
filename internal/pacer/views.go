package pacer

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"time"
)

func windowState(w observation.Window, now time.Time) string {
	state := w.State(now)
	if (state == "current" || state == "estimated") && !w.FreshComplete(now, time.Hour) {
		return "stale"
	}
	return state
}

// accountState is the weakest evidence among an account's windows. A dormant
// window (reset, not started) is as known as a current one, so the account is
// dormant only when none of its windows has started.
func accountState(provider string, windows []observation.Window, now time.Time) string {
	if len(windows) == 0 {
		return "unknown"
	}
	rank := map[string]int{"current": 0, "not-applicable": 0, "dormant": 0, "estimated": 2, "stale": 3, "partial": 4, "unknown": 5, "awaiting-confirmation": 6, "ambiguous": 7, "conflicting": 8}
	state, dormant, started := "current", false, false
	shared := map[int64]bool{}
	for _, w := range windows {
		candidate := windowState(w, now)
		dormant = dormant || candidate == "dormant"
		started = started || candidate == "current"
		if rank[candidate] > rank[state] {
			state = candidate
		}
		if w.Scope == "all" && !w.NotApplicable {
			shared[w.Seconds] = true
		}
	}
	if state == "current" && dormant && !started {
		state = "dormant"
	}
	if provider == "claude" && (!shared[18000] || !shared[604800]) && rank[state] < rank["partial"] {
		return "partial"
	}
	return state
}
