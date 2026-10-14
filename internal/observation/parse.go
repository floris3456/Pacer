package observation

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func providerName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "anthropic", "claude":
		return "claude"
	case "codex":
		return "codex"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func headersLower(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		if len(vs) > 0 {
			out[strings.ToLower(k)] = strings.TrimSpace(vs[0])
		}
	}
	return out
}

// decimalSeconds preserves nanosecond precision, unlike float64 Unix timestamps.
func decimalSeconds(raw string) (int64, int64, bool) {
	p := strings.Split(strings.TrimSpace(raw), ".")
	if len(p) > 2 || len(p) == 0 || p[0] == "" {
		return 0, 0, false
	}
	sec, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil || sec < 0 {
		return 0, 0, false
	}
	ns := int64(0)
	if len(p) == 2 {
		if p[1] == "" {
			return 0, 0, false
		}
		for _, c := range p[1] {
			if c < '0' || c > '9' {
				return 0, 0, false
			}
		}
		frac := p[1]
		if len(frac) > 9 {
			frac = frac[:9]
		}
		ns, err = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
	}
	return sec, ns, err == nil
}

func timestamp(raw string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
		return t.UTC()
	}
	sec, ns, ok := decimalSeconds(raw)
	if !ok || sec <= 0 || sec > 253402300799 {
		return time.Time{}
	}
	return time.Unix(sec, ns).UTC()
}

func relative(raw string, at time.Time) time.Time {
	s, ns, ok := decimalSeconds(raw)
	if !ok || at.IsZero() || s > int64(math.MaxInt64/int64(time.Second))-1 {
		return time.Time{}
	}
	return at.Add(time.Duration(s)*time.Second + time.Duration(ns)).UTC()
}

func scalar(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func window(provider, scope string, seconds int64, used, reset, after string, fraction bool, at time.Time, source string) Window {
	w := Window{Provider: provider, Scope: scope, Seconds: seconds}
	if u, err := strconv.ParseFloat(used, 64); err == nil && !math.IsNaN(u) && !math.IsInf(u, 0) {
		if fraction {
			u *= 100
		}
		if u >= 0 && u <= 100 {
			r := 100 - u
			w.Remaining = &r
			w.Usage = Field{at.UTC(), source, Measured, used}
		}
	}
	w.Reset = timestamp(reset)
	if !w.Reset.IsZero() {
		w.ResetEvidence = Field{at.UTC(), source, Absolute, reset}
	} else if after != "" {
		w.Reset = relative(after, at)
		if !w.Reset.IsZero() {
			w.ResetEvidence = Field{at.UTC(), source, Estimated, after}
		}
	}
	return w
}

func limitScope(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "codex", "codex_main", "codex-main", "all", "premium":
		// Codex labels its ordinary primary allowance "premium" in response
		// headers, while wham/usage leaves the same allowance unnamed.
		return "all"
	case "bengalfox", "codex_bengalfox", "codex-bengalfox", "spark", "gpt-5.3-codex-spark":
		return "spark"
	case "code-review", "code_review":
		return "code-review"
	default:
		return "limit:" + s
	}
}

func durationScope(name string) (int64, string) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 || i == len(name) {
		return 0, ""
	}
	n, err := strconv.ParseInt(name[:i], 10, 64)
	mult := int64(0)
	switch name[i] {
	case 'h':
		mult = 3600
	case 'd':
		mult = 86400
	}
	if err != nil || n <= 0 || mult == 0 || n > math.MaxInt64/mult {
		return 0, ""
	}
	scope := "all"
	if len(name) > i+1 {
		if name[i+1] != '-' && name[i+1] != '_' {
			return 0, ""
		}
		scope = strings.TrimLeft(name[i+1:], "-_")
		if scope == "" {
			return 0, ""
		}
		if scope == "oi" {
			scope = "fable"
		}
	}
	return n * mult, scope
}

// Headers is pure. at MUST be source response/event time, not delayed callback
// delivery time. It does not use Retry-After, RPM/TPM, or generic unified resets.
func Headers(provider string, h http.Header, at time.Time) []Window {
	provider = providerName(provider)
	if at.IsZero() {
		return nil
	}
	v := headersLower(h)
	prefixes := map[string]bool{}
	for key := range v {
		for _, suffix := range []string{"-utilization", "-used-percent", "-reset", "-reset-at", "-reset-after-seconds"} {
			if strings.HasSuffix(key, suffix) {
				prefixes[strings.TrimSuffix(key, suffix)] = true
			}
		}
	}
	out := []Window{}
	for prefix := range prefixes {
		var w Window
		switch provider {
		case "claude":
			if !strings.HasPrefix(prefix, "anthropic-ratelimit-unified-") {
				continue
			}
			seconds, scope := durationScope(strings.TrimPrefix(prefix, "anthropic-ratelimit-unified-"))
			w = window(provider, scope, seconds, v[prefix+"-utilization"], v[prefix+"-reset"], "", true, at, HeaderSource)
		case "codex":
			if !strings.HasPrefix(prefix, "x-codex-") {
				continue
			}
			base := strings.TrimPrefix(prefix, "x-codex-")
			if !strings.HasSuffix(base, "primary") && !strings.HasSuffix(base, "secondary") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimSuffix(base, "primary"), "secondary")
			name = strings.TrimSuffix(name, "-")
			if name == "" {
				name = v["x-codex-active-limit"]
			} else if strings.HasPrefix(name, "additional-") {
				if label := v["x-codex-"+name+"-limit-name"]; label != "" {
					name = label
				} else {
					name = strings.TrimPrefix(name, "additional-")
				}
			}
			minutes, _ := strconv.ParseInt(v[prefix+"-window-minutes"], 10, 64)
			if minutes <= 0 || minutes > math.MaxInt64/60 {
				continue
			}
			w = window(provider, limitScope(name), minutes*60, v[prefix+"-used-percent"], v[prefix+"-reset-at"], v[prefix+"-reset-after-seconds"], false, at, HeaderSource)
		default:
			continue
		}
		if w.Valid() {
			out = append(out, w)
		}
	}
	sortWindows(out)
	return out
}

func sortWindows(w []Window) {
	sort.SliceStable(w, func(i, j int) bool { return w[i].Key() < w[j].Key() })
}

func object(raw json.RawMessage) map[string]json.RawMessage {
	var v map[string]json.RawMessage
	_ = json.Unmarshal(raw, &v)
	return v
}

func first(v map[string]json.RawMessage, names ...string) json.RawMessage {
	for _, n := range names {
		if x, ok := v[n]; ok {
			return x
		}
	}
	return nil
}

// API parses verified Claude OAuth usage, Codex wham/usage and codex.rate_limits
// objects. Unsupported providers yield no observations. Unknown JSON metadata is
// ignored. Explicit Claude null/dormant values stay distinct from missing data.
func API(provider string, body []byte, at time.Time) ([]Window, error) {
	provider = providerName(provider)
	switch provider {
	case "antigravity":
		return antigravityAPI(body, at)
	case "opencode-go":
		return opencodeGoAPI(body, at)
	}
	if provider != "claude" && provider != "codex" {
		return nil, nil
	}
	if at.IsZero() {
		return nil, errors.New("observation: source time required")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("observation: expected quota object")
	}
	out := []Window{}
	if provider == "claude" {
		for name, raw := range data {
			seconds, scope := int64(604800), "all"
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
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				out = append(out, Window{Provider: provider, Scope: scope, Seconds: seconds, NotApplicable: true, StatusEvidence: Field{at.UTC(), APISource, Measured, "null"}})
				continue
			}
			v := object(raw)
			w := window(provider, scope, seconds, scalar(v["utilization"]), scalar(v["resets_at"]), "", false, at, APISource)
			if bytes.Equal(bytes.TrimSpace(v["resets_at"]), []byte("null")) && w.Remaining != nil && *w.Remaining == 100 {
				w.Dormant = true
				w.StatusEvidence = Field{at.UTC(), APISource, Measured, "null"}
			}
			if w.Valid() {
				out = append(out, w)
			}
		}
	} else {
		source := APISource
		if scalar(data["type"]) == "codex.rate_limits" {
			source = EventSource
		}
		add := func(scope string, raw json.RawMessage) {
			rate := object(raw)
			for _, names := range [][]string{{"primary_window", "primary"}, {"secondary_window", "secondary"}} {
				v := object(first(rate, names...))
				seconds, _ := strconv.ParseInt(scalar(v["limit_window_seconds"]), 10, 64)
				if seconds == 0 {
					minutes, _ := strconv.ParseInt(scalar(first(v, "window_minutes", "windowMinutes")), 10, 64)
					if minutes > 0 && minutes <= math.MaxInt64/60 {
						seconds = minutes * 60
					}
				}
				w := window(provider, scope, seconds, scalar(first(v, "used_percent", "usedPercent")), scalar(first(v, "reset_at", "resetAt")), scalar(first(v, "reset_after_seconds", "resetAfterSeconds")), false, at, source)
				if source == APISource {
					w = codexDormant(w, at)
				}
				if w.Valid() {
					out = append(out, w)
				}
			}
		}
		active := scalar(first(data, "metered_limit_name", "meteredLimitName", "limit_name", "limitName", "active_limit"))
		add(limitScope(active), first(data, "rate_limit", "rate_limits", "rateLimit"))
		add("code-review", first(data, "code_review_rate_limits", "codeReviewRateLimits"))
		additional := first(data, "additional_rate_limits", "additionalRateLimits")
		var list []json.RawMessage
		if json.Unmarshal(additional, &list) == nil {
			for _, raw := range list {
				v := object(raw)
				name := scalar(first(v, "limit_name", "limitName", "metered_feature", "name"))
				if name == "" {
					continue
				}
				rate := first(v, "rate_limit", "rateLimit")
				if len(rate) == 0 {
					rate = raw
				}
				add(limitScope(name), rate)
			}
		} else {
			for name, raw := range object(additional) {
				rate := first(object(raw), "rate_limit", "rateLimit")
				if len(rate) == 0 {
					rate = raw
				}
				add(limitScope(name), rate)
			}
		}
	}
	sortWindows(out)
	return out, nil
}

// CodexDormantTolerance bounds how far an unused Codex window's reported reset
// may be from read time + window length. Codex reports a window that has not
// started as resetting one full window after each read (0.1 s off on CT104);
// started windows were at least 52 minutes off.
const CodexDormantTolerance = 10 * time.Second

// codexDormant records a meter read of an unstarted Codex window as dormant,
// like Claude's resets_at: null, keeping the reported reset as status evidence.
// Response headers and stream events never qualify: a request starts the
// window, and its first minute also reads 0 % used.
func codexDormant(w Window, at time.Time) Window {
	if w.Remaining == nil || *w.Remaining != 100 || w.Reset.IsZero() || w.ResetEvidence.Quality != Absolute || w.Seconds <= 0 {
		return w
	}
	gap := w.Reset.Sub(at.Add(time.Duration(w.Seconds) * time.Second))
	if gap > CodexDormantTolerance || gap < -CodexDormantTolerance {
		return w
	}
	w.Dormant = true
	w.StatusEvidence = Field{at.UTC(), APISource, Measured, w.ResetEvidence.Raw}
	w.Reset, w.ResetEvidence = time.Time{}, Field{}
	return w
}

// MonthSeconds is the window length Pacer stores for monthly meters. The
// provider's month is a calendar month; the usage record derives its start
// from the reset, so the stored length only names the meter.
const MonthSeconds = 30 * 86400

// antigravityAPI parses Google's retrieveUserQuotaSummary answer: groups of
// models (Gemini; Claude and GPT) with a 5-hour and a weekly bucket each.
func antigravityAPI(body []byte, at time.Time) ([]Window, error) {
	if at.IsZero() {
		return nil, errors.New("observation: source time required")
	}
	var data struct {
		Groups []struct {
			DisplayName string            `json:"displayName"`
			Buckets     []json.RawMessage `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if data.Groups == nil {
		return nil, errors.New("observation: expected quota groups")
	}
	out := []Window{}
	for _, g := range data.Groups {
		name := strings.ToLower(g.DisplayName)
		scope := ""
		switch {
		case strings.Contains(name, "gemini"):
			scope = "gemini"
		case strings.Contains(name, "claude") && strings.Contains(name, "gpt"):
			scope = "claude_gpt"
		default:
			continue
		}
		for _, raw := range g.Buckets {
			v := object(raw)
			seconds := int64(0)
			switch scalar(v["window"]) {
			case "5h":
				seconds = 18000
			case "weekly":
				seconds = 604800
			default:
				continue
			}
			remaining, err := strconv.ParseFloat(scalar(v["remainingFraction"]), 64)
			if err != nil || math.IsNaN(remaining) || remaining < 0 || remaining > 1 {
				continue
			}
			w := window("antigravity", scope, seconds, strconv.FormatFloat(1-remaining, 'g', -1, 64), scalar(v["resetTime"]), "", true, at, APISource)
			if w.Valid() {
				out = append(out, w)
			}
		}
	}
	sortWindows(out)
	return out, nil
}

// opencodeGoAPI parses OpenCode Go's usage answer: rolling (5-hour), weekly
// and monthly windows, each with the used percent and its reset.
func opencodeGoAPI(body []byte, at time.Time) ([]Window, error) {
	if at.IsZero() {
		return nil, errors.New("observation: source time required")
	}
	var data struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if data.Usage == nil {
		return nil, errors.New("observation: expected usage windows")
	}
	out := []Window{}
	for name, seconds := range map[string]int64{"rolling": 18000, "weekly": 604800, "monthly": MonthSeconds} {
		v := object(data.Usage[name])
		if v == nil {
			continue
		}
		reset := scalar(first(v, "resetsAt", "resets_at"))
		w := window("opencode-go", "all", seconds, scalar(first(v, "percent", "usedPercent", "used_percent")), reset, scalar(first(v, "resetInSec", "reset_in_sec")), false, at, APISource)
		if w.Valid() {
			out = append(out, w)
		}
	}
	sortWindows(out)
	return out, nil
}
