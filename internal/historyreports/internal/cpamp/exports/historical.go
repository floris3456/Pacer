// Code extracted from CPAMP; Copyright (c) 2026 Seakee, MIT (see LICENSE).
// Controller CSV bodies; map-backed row groups have explicit deterministic order.
package exports

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	history "github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"sort"
	"strconv"
)

func WriteLegacyProfileReport(c *csv.Writer, report history.ProfileReport) {
	defer c.Flush()
	_ = c.Write([]string{"row_type", "key", "requests", "failed", "input", "output", "reasoning", "cached", "cache_creation", "total", "cost_nano_usd", "unpriced", "metadata_json"})
	write := func(kind, key string, t history.ProfileTotals, metadata any) {
		raw, _ := json.Marshal(metadata)
		_ = c.Write([]string{kind, safe(key), fmt.Sprint(t.Requests), fmt.Sprint(t.Failed), fmt.Sprint(t.Tokens.Input), fmt.Sprint(t.Tokens.Output), fmt.Sprint(t.Tokens.Reasoning), fmt.Sprint(t.Tokens.Cached), fmt.Sprint(t.Tokens.CacheCreation), fmt.Sprint(t.Tokens.Total), fmt.Sprint(t.CostNano), fmt.Sprint(t.Unpriced), string(raw)})
	}
	write("total", "selection", report.Totals, report.Filter)
	write("normalization", report.Normalization.ID, history.ProfileTotals{}, report.Normalization)
	for _, kind := range []string{"day", "model", "client", "profile"} {
		rows := map[string][]history.ProfileBucket{"day": report.Timeline, "model": report.Models, "client": report.Clients, "profile": report.ByProfile}[kind]
		for _, row := range rows {
			write(kind, row.Key, row.ProfileTotals, nil)
		}
	}
	for _, win := range report.Windows {
		write("quota_window", win.ID, history.ProfileTotals{Tokens: win.Tokens, Requests: win.Requests, Failed: win.Failed, CostNano: win.CostNano, Unpriced: win.Unpriced}, win)
	}
}

func WriteLegacyProfileProof(c *csv.Writer, p history.ProfileProof) {
	defer c.Flush()
	_ = c.Write([]string{"row_type", "id", "account", "model", "timestamp_ms", "requests", "input", "output", "total", "cost_nano_usd", "evidence_json"})
	for _, group := range []struct {
		name   string
		events []history.Event
	}{{"event", p.Events}, {"shared_event", p.SharedEvents}, {"boundary_event", p.BoundaryEvents}} {
		for _, e := range group.events {
			cost := ""
			if e.CostNano != nil {
				cost = fmt.Sprint(*e.CostNano)
			}
			raw, _ := json.Marshal(e)
			_ = c.Write([]string{group.name, e.Hash, safe(e.Account), safe(e.Model), fmt.Sprint(e.Started), "1", fmt.Sprint(e.Tokens.Input), fmt.Sprint(e.Tokens.Output), fmt.Sprint(e.Tokens.Total), cost, string(raw)})
		}
	}
	for _, o := range p.Observations {
		raw, _ := json.Marshal(o)
		_ = c.Write([]string{"observation", o.ID, safe(o.Account), "", fmt.Sprint(o.Observed), "", "", "", "", "", string(raw)})
	}
	ids := make([]string, 0, len(p.Prices))
	for id := range p.Prices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		price := p.Prices[id]
		_ = c.Write([]string{"price", id, "", "", "", "", "", "", "", "", string(price)})
	}
	for _, entry := range p.Forecasts {
		raw, _ := json.Marshal(entry.Forecast)
		_ = c.Write([]string{"forecast", entry.Forecast.ID, safe(entry.Forecast.Account), "", fmt.Sprint(entry.Forecast.Issued), "", "", "", "", "", string(raw)})
		for _, result := range entry.Results {
			raw, _ := json.Marshal(result)
			_ = c.Write([]string{"forecast_verification", result.ID, safe(entry.Forecast.Account), "", fmt.Sprint(result.Settled), "", "", "", "", "", string(raw)})
		}
	}
	for _, run := range p.PollRuns {
		raw, _ := json.Marshal(run)
		_ = c.Write([]string{"poll_attempt", fmt.Sprint(run.ID), "", "", fmt.Sprint(run.Observed), "", "", "", "", "", string(raw)})
	}
	for _, kind := range []string{"window", "filter", "normalization"} {
		value := map[string]any{"window": p.Window, "filter": p.Filter, "normalization": p.Normalization}[kind]
		raw, _ := json.Marshal(value)
		_ = c.Write([]string{kind, "", "", "", "", "", "", "", "", "", string(raw)})
	}
}

func WriteHistoryReport(c *csv.Writer, report history.Report) {
	defer c.Flush()
	_ = c.Write([]string{"window_id", "account", "provider", "kind", "scope", "start_ms", "reset_ms", "requests", "input", "output", "reasoning", "cached", "cache_creation", "total", "cost_nano_usd", "unpriced_requests", "last_used_percent", "peak_used_percent", "remaining_percent", "observed_at_ms", "straddling_requests", "coverage", "window_json"})
	for _, x := range report.Windows {
		raw, _ := json.Marshal(x)
		pct := func(p *float64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprint(*p)
		}
		_ = c.Write([]string{x.ID, safe(x.Label), safe(x.Provider), x.Kind, safe(x.Scope), fmt.Sprint(x.Start), fmt.Sprint(x.Reset), fmt.Sprint(x.Requests), fmt.Sprint(x.Tokens.Input), fmt.Sprint(x.Tokens.Output), fmt.Sprint(x.Tokens.Reasoning), fmt.Sprint(x.Tokens.Cached), fmt.Sprint(x.Tokens.CacheCreation), fmt.Sprint(x.Tokens.Total), fmt.Sprint(x.CostNano), fmt.Sprint(x.Unpriced), pct(x.Final), pct(x.Peak), pct(x.Remaining), fmt.Sprint(x.LastObserved), fmt.Sprint(x.Straddling), x.Coverage, string(raw)})
	}
}

func WriteHistoryProof(c *csv.Writer, proof history.Proof) {
	defer c.Flush()
	_ = c.Write([]string{"row_type", "id", "timestamp_ms", "source", "model", "input", "output", "reasoning", "cached", "cache_creation", "total", "cost_nano_usd", "used_percent", "reset_ms", "evidence_json"})
	for _, e := range proof.Events {
		raw, _ := json.Marshal(e)
		cost := ""
		if e.CostNano != nil {
			cost = strconv.FormatInt(*e.CostNano, 10)
		}
		_ = c.Write([]string{"event", strconv.FormatInt(e.ID, 10), strconv.FormatInt(e.Started, 10), "usage_events", safe(e.Model), fmt.Sprint(e.Tokens.Input), fmt.Sprint(e.Tokens.Output), fmt.Sprint(e.Tokens.Reasoning), fmt.Sprint(e.Tokens.Cached), fmt.Sprint(e.Tokens.CacheCreation), fmt.Sprint(e.Tokens.Total), cost, "", "", string(raw)})
	}
	for _, e := range proof.SharedEvents {
		raw, _ := json.Marshal(e)
		cost := ""
		if e.CostNano != nil {
			cost = strconv.FormatInt(*e.CostNano, 10)
		}
		_ = c.Write([]string{"shared_event", strconv.FormatInt(e.ID, 10), strconv.FormatInt(e.Started, 10), "usage_events", safe(e.Model), fmt.Sprint(e.Tokens.Input), fmt.Sprint(e.Tokens.Output), fmt.Sprint(e.Tokens.Reasoning), fmt.Sprint(e.Tokens.Cached), fmt.Sprint(e.Tokens.CacheCreation), fmt.Sprint(e.Tokens.Total), cost, "", "", string(raw)})
	}
	for _, o := range proof.Observations {
		raw, _ := json.Marshal(o)
		used := ""
		if o.Used != nil {
			used = fmt.Sprint(*o.Used)
		}
		_ = c.Write([]string{"observation", o.ID, fmt.Sprint(o.Observed), o.Source, "", "", "", "", "", "", "", "", used, fmt.Sprint(o.Reset), string(raw)})
	}
	ids := make([]string, 0, len(proof.Prices))
	for id := range proof.Prices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := proof.Prices[id]
		_ = c.Write([]string{"price", id, "", "list_price", "", "", "", "", "", "", "", "", "", "", string(p)})
	}
	for _, run := range proof.PollRuns {
		raw, _ := json.Marshal(run)
		_ = c.Write([]string{"poll_attempt", fmt.Sprint(run.ID), fmt.Sprint(run.Observed), run.Source, "", "", "", "", "", "", "", "", "", "", string(raw)})
	}
	raw, _ := json.Marshal(proof.Window)
	_ = c.Write([]string{"window", proof.Window.ID, "", "derived", "", "", "", "", "", "", "", "", "", "", string(raw)})
}

func WriteQuotaUsage(c *csv.Writer, report history.QuotaUsageReport, f history.QuotaUsageFilter) {
	defer c.Flush()
	_ = c.Write([]string{"window_id", "credential", "provider", "kind", "scope", "start_ms", "reset_ms", "observed_ms", "account_quota_percent", "key_quota_percent", "attribution_status", "requests", "tokens", "api_equivalent_nano_usd", "unpriced_requests", "source_plan", "price_basis"})
	for _, v := range report.Windows {
		if f.WindowID != "" && f.WindowID != v.ID {
			continue
		}
		used, key := "", ""
		if v.Used != nil {
			used = strconv.FormatFloat(*v.Used, 'f', -1, 64)
		}
		if f.APIKey != "" && v.Attribution.Percent != nil {
			key = strconv.FormatFloat(*v.Attribution.Percent, 'f', -1, 64)
		}
		_ = c.Write([]string{v.ID, safe(v.Label), safe(v.Provider), safe(v.Kind), safe(v.Scope), fmt.Sprint(v.Start), fmt.Sprint(v.Reset), fmt.Sprint(v.LastObserved), used, key, v.Attribution.Status, fmt.Sprint(v.Selected.Requests), fmt.Sprint(v.Selected.Tokens.Total), fmt.Sprint(v.Selected.CostNano), fmt.Sprint(v.Selected.Unpriced), safe(v.SourcePlan), "captured event prices"})
	}
}
