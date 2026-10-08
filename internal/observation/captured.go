package observation

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// HeadersCaptured retains each field's actual response/event receive clock.
// Later event fields cannot re-anchor an older countdown or turn an older balance
// into fresh evidence. Missing capture clocks mean missing evidence, not "now".
func HeadersCaptured(provider string, h http.Header, captured map[string]time.Time) []Window {
	clocks := map[string]time.Time{}
	for key, at := range captured {
		clocks[strings.ToLower(key)] = at
	}
	groups := map[time.Time]http.Header{}
	context := http.Header{}
	for key, values := range h {
		key = strings.ToLower(key)
		if strings.HasSuffix(key, "-window-minutes") || strings.HasSuffix(key, "-limit-name") || key == "x-codex-active-limit" {
			context[key] = values
			continue
		}
		at := clocks[key]
		if at.IsZero() {
			continue
		}
		at = at.UTC()
		if groups[at] == nil {
			groups[at] = http.Header{}
		}
		groups[at][key] = values
	}
	times := []time.Time{}
	for at := range groups {
		times = append(times, at)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	result := []Window{}
	for _, at := range times {
		for key, values := range context {
			clock := clocks[key]
			// A newer scope/duration cannot be assigned backwards to an old
			// field. A known older dimension remains valid for a partial update.
			if !clock.IsZero() && !clock.After(at) {
				groups[at][key] = values
				continue
			}
			if key == "x-codex-active-limit" {
				for field := range groups[at] {
					if strings.HasPrefix(field, "x-codex-primary-") || strings.HasPrefix(field, "x-codex-secondary-") {
						delete(groups[at], field)
					}
				}
			} else if strings.HasSuffix(key, "-limit-name") {
				prefix := strings.TrimSuffix(key, "-limit-name") + "-"
				for field := range groups[at] {
					if strings.HasPrefix(field, prefix) {
						delete(groups[at], field)
					}
				}
			}
		}
		result = append(result, Headers(provider, groups[at], at)...)
	}
	return result
}
