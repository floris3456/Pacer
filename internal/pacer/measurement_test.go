package pacer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

func TestDelayedNativeUsageKeepsItsCapturedAccountAndSourceAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	c := p.creds["a"]
	source := p.measurementSource
	if err := p.captureUsageCredential("logical", c); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p = startPlugin(t, path)
	defer p.Close()
	if p.measurementSource != source {
		t.Fatal("stable event source changed on restart")
	}
	curr := c
	curr.Account = "different-current-account"
	p.mu.Lock()
	p.creds["a"] = curr
	p.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"Provider": "claude", "Model": "claude-opus-5-5", "LifecycleRequestID": "logical", "RequestID": "physical", "AuthID": c.ID, "AuthIndex": c.Index, "APIKey": "private-key-never-persist", "RequestHeaders": map[string]string{"Authorization": "Bearer private-never-persist"}, "RequestedAt": time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), "Latency": int64(time.Second), "Failed": false, "AccountingVersion": 2, "Detail": map[string]any{}, "TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": 12, "input": map[string]int{"total_tokens": 10, "uncached_tokens": 10, "cache_read_tokens": 0, "cache_write_tokens": 0}, "output": map[string]int{"total_tokens": 2, "non_reasoning_tokens": 2, "reasoning_tokens": 0}, "unclassified_tokens": 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.recordUsage(raw); err != nil {
		t.Fatal(err)
	}
	if err = p.recordUsage(raw); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = p.db.QueryRow(`SELECT count(*) FROM measurement_records`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var body []byte
	if err = p.db.QueryRow(`SELECT body FROM measurement_records`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record measurement.Record
	if err = json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if record.Attribution.AccountID != c.Account || record.AttemptID != "physical" || record.RequestID != "logical" {
		t.Fatal(record.Attribution, record.AttemptID, record.RequestID)
	}
	if strings.Contains(string(body), "private-") {
		t.Fatal("private usage input persisted")
	}
	p.refreshMeasurements()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, report := range p.measurements {
		if report.Coverage.Observed || report.SevenDay.Known || report.FiveHour.Known {
			t.Fatal("callback invented drained coverage", report.Coverage)
		}
	}
}
