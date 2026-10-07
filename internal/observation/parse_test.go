package observation

import (
	"net/http"
	"testing"
	"time"
)

func instant(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestHeadersPartialFullAndNoSignal(t *testing.T) {
	at := instant("2026-10-07T07:34:00Z")
	tests := []struct {
		name, provider string
		h              http.Header
		count          int
		usage, reset   bool
	}{
		{"claude-full", "claude", http.Header{"anthropic-ratelimit-unified-5h-utilization": {"0.25"}, "anthropic-ratelimit-unified-5h-reset": {"1791358440.123456789"}}, 1, true, true},
		{"claude-percent", "claude", http.Header{"Anthropic-Ratelimit-Unified-7d-Utilization": {"0"}}, 1, true, false},
		{"claude-reset", "claude", http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {"1791958440"}}, 1, false, true},
		{"codex-percent-weekly-primary", "codex", http.Header{"X-Codex-Primary-Used-Percent": {"81"}, "X-Codex-Primary-Window-Minutes": {"10080"}}, 1, true, false},
		{"codex-reset-daily", "codex", http.Header{"X-Codex-Primary-Reset-At": {"1791358440"}, "X-Codex-Primary-Window-Minutes": {"1440"}}, 1, false, true},
		{"codex-unknown-duration", "codex", http.Header{"X-Codex-Primary-Used-Percent": {"81"}}, 0, false, false},
		{"generic-rpm", "codex", http.Header{"X-Ratelimit-Remaining-Requests": {"0"}, "Retry-After": {"120"}}, 0, false, false},
		{"overage-retry", "claude", http.Header{"Anthropic-Ratelimit-Unified-Overage-Reset": {"1791358440"}, "Retry-After": {"120"}}, 0, false, false},
		{"unsupported", "opencode-go", http.Header{"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"50"}}, 0, false, false},
		{"malformed", "claude", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"NaN"}, "Anthropic-Ratelimit-Unified-5h-Reset": {"tomorrow"}}, 0, false, false},
		{"no-signal", "claude", http.Header{"X-Request-Id": {"fixture"}}, 0, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := Headers(tc.provider, tc.h, at)
			if len(w) != tc.count {
				t.Fatalf("got %#v", w)
			}
			if len(w) > 0 && ((w[0].Remaining != nil) != tc.usage || (!w[0].Reset.IsZero()) != tc.reset) {
				t.Fatalf("fields %#v", w[0])
			}
		})
	}
}

func TestFractionalTimestampsAndSourceAnchoring(t *testing.T) {
	at := instant("2026-10-07T07:34:00.987654321Z")
	w := Headers("claude", http.Header{"Anthropic-Ratelimit-Unified-7d-Reset": {"1791958440.123456789"}}, at)[0]
	if w.Reset.Nanosecond() != 123456789 || w.ResetEvidence.Raw != "1791958440.123456789" {
		t.Fatalf("lost precision: %#v", w)
	}
	a, err := API("claude", []byte(`{"seven_day":{"utilization":7,"resets_at":"2026-10-14T09:34:00.123456789+02:00"}}`), at)
	if err != nil || len(a) != 1 || a[0].Reset.Format(time.RFC3339Nano) != "2026-10-14T07:34:00.123456789Z" {
		t.Fatalf("RFC3339: %#v %v", a, err)
	}
	h := http.Header{"X-Codex-Primary-Window-Minutes": {"17"}, "X-Codex-Primary-Used-Percent": {"7"}, "X-Codex-Primary-Reset-After-Seconds": {"90.25"}, "Date": {"Wed, 07 Oct 2026 07:34:00 GMT"}}
	r := Headers("codex", h, at)[0]
	if r.Seconds != 1020 || !r.Reset.Equal(at.Add(90250*time.Millisecond)) || r.ResetEvidence.Quality != Estimated {
		t.Fatalf("relative: %#v", r)
	}
	if len(Headers("codex", h, time.Time{})) != 0 {
		t.Fatal("relative reset requires a source anchor")
	}
}

// The event structure and weekly-primary values are from the audited CPA
// observed-production fixture. Relative reset timing is anchored by this test.
func TestCodexRecordedEventScopesAndArbitraryDurations(t *testing.T) {
	at := instant("2026-08-22T00:00:00Z")
	body := []byte(`{
	 "type":"codex.rate_limits",
	 "rate_limits":{"allowed":true,"primary":{"used_percent":81,"window_minutes":10080,"reset_after_seconds":137160,"reset_at":1787588999},"secondary":null},
	 "code_review_rate_limits":{"primary":{"used_percent":4,"window_minutes":300,"reset_after_seconds":900}},
	 "additional_rate_limits":{"GPT-5.3-Codex-Spark":{"primary":{"used_percent":0,"window_minutes":300,"reset_after_seconds":18000}},"custom-gpu":{"primary":{"used_percent":5,"window_minutes":1440,"reset_after_seconds":120}}}
	}`)
	w, err := API("codex", body, at)
	if err != nil || len(w) != 4 {
		t.Fatalf("%#v %v", w, err)
	}
	byScope := map[string]Window{}
	for _, x := range w {
		byScope[x.Scope] = x
		if x.Usage.Source != EventSource {
			t.Fatal("event provenance lost")
		}
	}
	if byScope["all"].Seconds != 604800 || *byScope["all"].Remaining != 19 {
		t.Fatal("weekly primary was relabelled")
	}
	if !byScope["spark"].Applies("gpt-5.3-codex-spark") || byScope["spark"].Applies("gpt-5.3-codex") || byScope["code-review"].Applies("gpt-5.3-codex") {
		t.Fatal("scope leakage")
	}
	if byScope["limit:custom-gpu"].Seconds != 86400 {
		t.Fatal("arbitrary limit was discarded")
	}
	h := http.Header{"X-Codex-Active-Limit": {"codex_bengalfox"}, "X-Codex-Primary-Used-Percent": {"1"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"60"}, "X-Codex-Additional-Gpt-5.3-Codex-Spark-Limit-Name": {"GPT-5.3-Codex-Spark"}, "X-Codex-Additional-Gpt-5.3-Codex-Spark-Secondary-Used-Percent": {"2"}, "X-Codex-Additional-Gpt-5.3-Codex-Spark-Secondary-Window-Minutes": {"1440"}, "X-Codex-Additional-Gpt-5.3-Codex-Spark-Secondary-Reset-At": {"1787588999"}}
	w = Headers("codex", h, at)
	if len(w) != 2 || w[0].Scope != "spark" || w[1].Scope != "spark" {
		t.Fatalf("active/additional namespace: %#v", w)
	}
}

func TestAPIPartialNullDormantAndMalformed(t *testing.T) {
	at := instant("2026-10-07T00:00:00Z")
	w, err := API("claude", []byte(`{"five_hour":{"utilization":20},"seven_day":{"resets_at":"2026-10-14T07:34:00Z"},"seven_day_opus":null,"seven_day_sonnet":{"utilization":0,"resets_at":null},"seven_day_oauth_apps":{"utilization":0},"extra_usage":{"is_enabled":true,"used_credits":10},"ignored":true}`), at)
	if err != nil || len(w) != 5 {
		t.Fatalf("%#v %v", w, err)
	}
	byScope := map[string]Window{}
	for _, x := range w {
		byScope[x.Scope] = x
	}
	if !byScope["opus"].NotApplicable || byScope["opus"].Remaining != nil || !byScope["sonnet"].Dormant || byScope["oauth"].Dormant {
		t.Fatal("missing, null and dormant conflated")
	}
	for _, body := range []string{`{}`, `{"rate_limit":{"allowed":true,"primary_window":null,"secondary_window":null}}`, `{"rate_limit":{"primary_window":{"used_percent":-1,"limit_window_seconds":86400,"reset_at":"bad"}}}`} {
		w, err = API("codex", []byte(body), at)
		if err != nil || len(w) != 0 {
			t.Fatalf("invented data: %s %#v %v", body, w, err)
		}
	}
	if _, err := API("codex", []byte(`{"broken"`), at); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	w, err = API("antigravity", []byte(`{"remainingFraction":1}`), at)
	if err != nil || len(w) != 0 {
		t.Fatal("unverified provider parsed")
	}
}

func TestClaudeModelAndOverageScopes(t *testing.T) {
	w := Headers("claude", http.Header{"Anthropic-Ratelimit-Unified-7d-Opus-Utilization": {"0.9"}, "Anthropic-Ratelimit-Unified-7d-Overage-Reset": {"1791958440"}, "Anthropic-Ratelimit-Unified-7d_oi-Reset": {"1791958440"}}, instant("2026-10-07T00:00:00Z"))
	if len(w) != 3 {
		t.Fatalf("%#v", w)
	}
	for _, x := range w {
		if x.Scope == "opus" && (!x.Applies("claude-opus-4-6") || x.Applies("claude-sonnet-4-6")) {
			t.Fatal("opus scope")
		}
		if x.Scope != "opus" && x.Applies("claude-opus-4-6") {
			t.Fatal("overage/fable leaked into ordinary model budget")
		}
	}
}
