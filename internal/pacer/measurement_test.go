package pacer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

// A request that wrote a long chat's cache moved the meter after its own reply
// was read: its account's meter is read again (observation.Spend). A request
// below meterRereadTokens of uncached input asks for nothing.
func TestLargeRequestAsksForAMeterRead(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	c := p.creds["a"]
	usage := func(id string, uncached, read, write int) {
		t.Helper()
		if err := p.captureUsageCredential(id, c); err != nil {
			t.Fatal(err)
		}
		input := uncached + read + write
		raw, err := json.Marshal(map[string]any{"Provider": "claude", "Model": "claude-opus-5-5", "LifecycleRequestID": id, "RequestID": id + "-physical", "AuthID": c.ID, "AuthIndex": c.Index, "RequestedAt": p.now().Add(-18 * time.Second), "Latency": int64(18 * time.Second), "Failed": false, "AccountingVersion": 2, "Detail": map[string]any{}, "TokenBreakdown": map[string]any{"schema_version": 2, "quality": "complete", "total_tokens": input + 4, "input": map[string]int{"total_tokens": input, "uncached_tokens": uncached, "cache_read_tokens": read, "cache_write_tokens": write}, "output": map[string]int{"total_tokens": 4, "non_reasoning_tokens": 4, "reasoning_tokens": 0}, "unclassified_tokens": 0}})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.recordUsage(raw); err != nil {
			t.Fatal(err)
		}
	}
	// This chat's step after its compaction (2026-10-15): 34k written.
	usage("step", 2, 4585, 34121)
	if j, exists, err := p.observer.Status(c.Account); err != nil || exists && !j.Activity.IsZero() {
		t.Fatal("a small request asked for a meter read", j, err)
	}
	// The other chat's warming ping that rewrote its cache on another account.
	usage("ping", 4, 4585, 573843)
	j, exists, err := p.observer.Status(c.Account)
	if err != nil || !exists || j.Activity.IsZero() || j.Endpoint != "claude:usage" {
		t.Fatal("a large request asked for no meter read", j, exists, err)
	}
}

func TestDelayedNativeUsageKeepsItsCapturedAccountAndSourceAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	c := p.creds["a"]
	source := p.measurementSource
	if err := p.captureUsageCredential("logical", c); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.Account = "replacement-account"
	if err := p.captureUsageCredential("logical", changed); err == nil {
		t.Fatal("a retry rebound immutable account identity")
	}
	if err := p.captureUsageCredential("logical", c); err != nil {
		t.Fatal("original identity retry refused", err)
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
