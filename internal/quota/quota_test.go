package quota

import (
	"net/http"
	"testing"
)

func TestThresholdsAndVerifiedReset(t *testing.T) {
	for _, duration := range []int64{18000, 604800} {
		warning, handoff := (Window{Seconds: duration}).Thresholds()
		first := Window{Scope: "all", Seconds: duration, Remaining: warning, Reset: 100000, Observed: 90000}
		first = Merge(Window{}, first)
		if !first.Warned || first.Held {
			t.Fatal(first)
		}
		low := first
		low.Remaining = handoff
		low.Observed++
		low = Merge(first, low)
		if !low.Held {
			t.Fatal(low)
		}
		jitter := low
		jitter.Remaining = 90
		jitter.Observed = 100001
		jitter.Reset += 120
		if !Merge(low, jitter).Held {
			t.Fatal("clock correction released a hold")
		}
		reset := jitter
		reset.Reset = low.Reset + duration
		next := Merge(low, reset)
		if next.Held || !next.Reclaimed || next.Warned {
			t.Fatal(next)
		}
		if Merge(low, first).Held != true {
			t.Fatal("old observation released hold")
		}
	}
}
func TestProviderUnitsAndWindowDurations(t *testing.T) {
	claude := Headers("claude", http.Header{"anthropic-ratelimit-unified-5h-utilization": {"0.8"}, "anthropic-ratelimit-unified-5h-reset": {"100000"}}, 90000)
	if len(claude) != 1 || claude[0].Remaining != 20 {
		t.Fatal(claude)
	}
	codex := Headers("codex", http.Header{"x-codex-secondary-used-percent": {"95"}, "x-codex-secondary-window-minutes": {"300"}, "x-codex-secondary-reset-at": {"100000"}}, 90000)
	if len(codex) != 1 || codex[0].Remaining != 5 || codex[0].Seconds != 18000 {
		t.Fatal(codex)
	}
}
func TestAPIScopesAndInvalidEvidence(t *testing.T) {
	windows, err := API("claude", []byte(`{"five_hour":{"utilization":80,"resets_at":"2026-10-01T12:00:00Z"},"seven_day_opus":{"utilization":95,"resets_at":"2026-10-05T12:00:00Z"},"seven_day":null}`), 100000)
	if err != nil || len(windows) != 3 {
		t.Fatal(windows, err)
	}
	if (Window{Scope: "opus"}).Applies("claude-sonnet-4-6") {
		t.Fatal("model scope leaked")
	}
	windows, err = API("codex", []byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"used_percent":85,"reset_at":100000},"secondary_window":{"limit_window_seconds":18000,"used_percent":null,"reset_at":100000}}}`), 90000)
	if err != nil || len(windows) != 1 || windows[0].Seconds != 604800 {
		t.Fatal(windows, err)
	}
	if len(Headers("claude", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"NaN"}}, 90000)) != 0 {
		t.Fatal("invalid evidence accepted")
	}
}

func TestLiveProviderWindowShapes(t *testing.T) {
	windows, err := API("claude", []byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":86,"resets_at":"2026-10-04T21:59:59.595334+00:00"}}`), 1790830000)
	if err != nil || len(windows) != 2 {
		t.Fatal(windows, err)
	}
	for _, w := range windows {
		if w.Seconds == 18000 && (!w.Dormant || w.Remaining != 100 || w.Reset != 0 || !w.Valid()) {
			t.Fatal(w)
		}
	}
	windows, err = API("codex", []byte(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":61,"limit_window_seconds":604800,"reset_at":1791046700},"secondary_window":null}}`), 1790830000)
	if err != nil || len(windows) != 2 {
		t.Fatal(windows, err)
	}
	for _, w := range windows {
		if w.Seconds == 18000 && (!w.NotApplicable || Merge(Window{}, w).Held) {
			t.Fatal(w)
		}
	}
	missing, _ := API("codex", []byte(`{"rate_limit":{"allowed":true}}`), 1790830000)
	if len(missing) != 0 {
		t.Fatal("missing data was converted to capacity", missing)
	}
	old := Window{Scope: "all", Seconds: 18000, Remaining: 9, Reset: 1000, Observed: 900, Held: true, Warned: true}
	dormant := Window{Scope: "all", Seconds: 18000, Remaining: 100, Observed: 1001, Source: "api", Dormant: true}
	if next := Merge(old, dormant); next.Held || !next.Reclaimed || next.Reset != 0 {
		t.Fatal(next)
	}
}
