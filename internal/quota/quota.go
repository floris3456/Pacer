// Package quota normalizes provider evidence. It never estimates quota from tokens or money.
package quota

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Window struct {
	Covered       int64   `json:"covered_through,omitempty"`
	Dormant       bool    `json:"dormant,omitempty"`
	NotApplicable bool    `json:"not_applicable,omitempty"`
	Scope         string  `json:"scope"`
	Seconds       int64   `json:"seconds"`
	Remaining     float64 `json:"remaining"`
	Reset         int64   `json:"reset"`
	Observed      int64   `json:"observed"`
	Source        string  `json:"source"`
	Held          bool    `json:"held"`
	Warned        bool    `json:"warned"`
	Reclaimed     bool    `json:"reclaimed"`
	Cycle         int64   `json:"cycle"`
}

func (w Window) Key() string { return w.Scope + ":" + strconv.FormatInt(w.Seconds, 10) }
func (w Window) Thresholds() (float64, float64) {
	if w.Seconds == 18000 {
		return 20, 10
	}
	return 15, 5
}
func (w Window) Applies(model string) bool {
	return w.Scope == "all" || strings.Contains(strings.ToLower(model), w.Scope)
}
func (w Window) Valid() bool {
	return (w.Seconds == 18000 || w.Seconds == 604800) && w.Scope != "" &&
		!math.IsNaN(w.Remaining) && !math.IsInf(w.Remaining, 0) && w.Remaining >= 0 && w.Remaining <= 100 && (w.Reset > 0 || (w.Source == "api" && (w.NotApplicable || (w.Dormant && w.Remaining == 100)))) && w.Observed > 0
}

// Reset-time corrections do not release holds. Only a post-boundary observation
// reporting a later cycle can do so. Old/out-of-order samples never rearm notices.
func Merge(old, next Window) Window {
	if !next.Valid() || next.Observed < old.Observed {
		return old
	}
	next.Held, next.Warned = false, false
	if next.NotApplicable {
		next.Reclaimed = old.Reclaimed || old.Held
		next.Cycle = next.Observed
		return next
	}
	reset := old.Reset > 0 && next.Observed >= old.Reset && (next.Dormant || next.Reset > old.Reset+old.Seconds/2)
	next.Cycle = old.Cycle
	next.Reclaimed = old.Reclaimed || (reset && old.Held)
	if old.Reset == 0 || reset {
		next.Cycle = next.Reset
		if next.Dormant {
			next.Cycle = next.Observed
		}
	} else {
		next.Held = old.Held
		next.Warned = old.Warned
		// A shifted countdown is not evidence of a new cycle.
		if old.Held {
			next.Reset = old.Reset
		}
	}
	warning, handoff := next.Thresholds()
	next.Held = next.Held || next.Remaining <= handoff
	next.Warned = next.Warned || next.Remaining <= warning
	return next
}

func Headers(provider string, headers http.Header, now int64) []Window {
	h := http.Header{}
	for k, v := range headers {
		h[http.CanonicalHeaderKey(k)] = v
	}
	out := []Window{}
	add := func(scope string, duration int64, used, reset, source string, fraction bool) {
		u, err := strconv.ParseFloat(used, 64)
		if err != nil {
			return
		}
		if fraction {
			u *= 100
		}
		r, err := strconv.ParseInt(reset, 10, 64)
		if err != nil {
			return
		}
		w := Window{Scope: scope, Seconds: duration, Remaining: 100 - u, Reset: r, Observed: now, Source: source}
		if w.Valid() {
			out = append(out, w)
		}
	}
	if provider == "claude" {
		for _, p := range []struct {
			name, scope string
			seconds     int64
		}{{"5h", "all", 18000}, {"7d", "all", 604800}, {"7d-opus", "opus", 604800}, {"7d-sonnet", "sonnet", 604800}} {
			prefix := "anthropic-ratelimit-unified-" + p.name
			add(p.scope, p.seconds, h.Get(prefix+"-utilization"), h.Get(prefix+"-reset"), "header", true)
		}
	}
	if provider == "codex" {
		for k := range h {
			key := strings.ToLower(k)
			if !strings.HasPrefix(key, "x-codex-") || !strings.HasSuffix(key, "-used-percent") {
				continue
			}
			prefix := strings.TrimSuffix(key, "-used-percent")
			minutes, _ := strconv.ParseInt(h.Get(prefix+"-window-minutes"), 10, 64)
			scope := "all"
			if strings.Contains(prefix, "spark") {
				scope = "spark"
			} else if prefix != "x-codex-primary" && prefix != "x-codex-secondary" {
				continue
			}
			reset := h.Get(prefix + "-reset-at")
			if reset == "" {
				if seconds, err := strconv.ParseInt(h.Get(prefix+"-reset-after-seconds"), 10, 64); err == nil {
					reset = strconv.FormatInt(now+seconds, 10)
				}
			}
			add(scope, minutes*60, h.Get(k), reset, "header", false)
		}
	}
	return out
}

// Explicit API nulls describe non-applicable windows; a zero-utilization
// window with no reset is dormant. Neither case invents a future reset time.
func API(provider string, body []byte, now int64) ([]Window, error) {
	out := []Window{}
	if provider == "claude" {
		// The live endpoint also returns booleans, arrays and billing metadata.
		// Decode only recognized quota keys; unrelated shapes are not windows.
		var data map[string]json.RawMessage
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, err
		}
		for name, raw := range data {
			seconds := int64(604800)
			scope := "all"
			switch name {
			case "five_hour":
				seconds = 18000
			case "seven_day":
			case "seven_day_opus":
				scope = "opus"
			case "seven_day_sonnet":
				scope = "sonnet"
			case "seven_day_oauth_apps":
				scope = "oauth"
			default:
				continue
			}
			w := Window{Scope: scope, Seconds: seconds, Observed: now, Source: "api"}
			if strings.TrimSpace(string(raw)) == "null" {
				w.NotApplicable = true
				out = append(out, w)
				continue
			}
			var v struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    *string  `json:"resets_at"`
			}
			if json.Unmarshal(raw, &v) != nil {
				continue
			}
			if v.Utilization == nil {
				continue
			}
			w.Remaining = 100 - *v.Utilization
			if v.ResetsAt == nil && *v.Utilization == 0 {
				w.Dormant = true
			} else if v.ResetsAt != nil {
				reset, err := time.Parse(time.RFC3339, *v.ResetsAt)
				if err != nil {
					continue
				}
				w.Reset = reset.Unix()
			}
			if w.Valid() {
				out = append(out, w)
			}
		}
		return out, nil
	}
	type codexWindow struct {
		Used    *float64 `json:"used_percent"`
		Seconds int64    `json:"limit_window_seconds"`
		Reset   int64    `json:"reset_at"`
		After   int64    `json:"reset_after_seconds"`
	}
	type rate struct {
		Allowed   *bool           `json:"allowed"`
		Primary   json.RawMessage `json:"primary_window"`
		Secondary json.RawMessage `json:"secondary_window"`
	}
	var data struct {
		Rate       rate `json:"rate_limit"`
		Additional []struct {
			Name  string `json:"limit_name"`
			Meter string `json:"metered_feature"`
			Rate  rate   `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	add := func(scope string, r rate) {
		present := map[int64]bool{}
		complete := len(r.Primary) > 0 && len(r.Secondary) > 0 && r.Allowed != nil && *r.Allowed
		for _, raw := range []json.RawMessage{r.Primary, r.Secondary} {
			if string(raw) == "null" {
				continue
			}
			var v codexWindow
			if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v.Used == nil {
				complete = false
				continue
			}
			reset := v.Reset
			if reset == 0 && v.After > 0 {
				reset = now + v.After
			}
			w := Window{Scope: scope, Seconds: v.Seconds, Remaining: 100 - *v.Used, Reset: reset, Observed: now, Source: "api"}
			if w.Valid() {
				out = append(out, w)
				present[v.Seconds] = true
			} else {
				complete = false
			}
		}
		if complete {
			for _, seconds := range []int64{18000, 604800} {
				if !present[seconds] {
					out = append(out, Window{Scope: scope, Seconds: seconds, NotApplicable: true, Observed: now, Source: "api"})
				}
			}
		}
	}
	add("all", data.Rate)
	for _, v := range data.Additional {
		if strings.Contains(strings.ToLower(v.Name+v.Meter), "spark") {
			add("spark", v.Rate)
		}
	}
	return out, nil
}
