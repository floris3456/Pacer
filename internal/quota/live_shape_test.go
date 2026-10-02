package quota

import "testing"

func TestClaudeIgnoresNonWindowLiveMetadata(t *testing.T) {
	for _, metadata := range []string{`"member_dashboard_available":true`, `"seven_day_breakdown":[{"label":"workload"}]`, `"spend":23.5`, `"limits":{"value":true}`} {
		body := []byte(`{"five_hour":{"utilization":82,"resets_at":"2027-01-01T10:00:00Z"},"seven_day":{"utilization":12,"resets_at":"2027-01-05T10:00:00Z"},"seven_day_opus":null,` + metadata + `}`)
		windows, err := API("claude", body, 1798797600)
		if err != nil || len(windows) != 3 {
			t.Fatalf("live metadata hid valid windows: %+v %v", windows, err)
		}
		found := false
		for _, w := range windows {
			if w.Seconds == 18000 && w.Remaining == 18 {
				found = true
			}
		}
		if !found {
			t.Fatalf("five-hour reading missing: %+v", windows)
		}
	}
}
