package pacer

import (
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"testing"
)

func TestWindowSelectionStrictInput(t *testing.T) {
	for _, raw := range []string{`{"latest_only":true,"selected_windows":{}}`, `{"selected_windows":{"profile:one":["window-one"]}}`} {
		var q historyreports.CompareQuery
		if err := decodeStrictReport([]byte(raw), &q); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{`{"selected_windows":null}`, `{"selected_windows":{"p":[],"p":[]}}`, `{"selected_windows":{"p":[{}]}}`, `{"selected_windows":{"p":"window"}}`, `{"Selected_Windows":{}}`, `{"latest_only":true,"latest_only":false}`} {
		var q historyreports.CompareQuery
		if err := decodeStrictReport([]byte(raw), &q); err == nil {
			t.Fatal("invalid selection accepted", raw)
		}
	}
}
