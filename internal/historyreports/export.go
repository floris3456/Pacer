package historyreports

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/exports"
)

type ExportOptions struct {
	WindowID          string
	CSV               string
	Kind              string
	ProfileReferences []string
}

// ExportJSON keeps the original CPAMP object shape, encoding and report pins.
func (r *Reader) ExportJSON(ctx context.Context, q Query, o ExportOptions, w io.Writer) error {
	if o.WindowID != "" || q.EvidencePin != nil {
		p, e := r.Evidence(ctx, q, o.WindowID)
		if e != nil {
			return e
		}
		return json.NewEncoder(w).Encode(p)
	}
	p, e := r.Report(ctx, q)
	if e != nil {
		return e
	}
	return json.NewEncoder(w).Encode(p)
}

// ExportCSV supports the active daily, comparison and quota-evidence exports.
// Existing leading columns, JSON cells and provenance columns are unchanged.
func (r *Reader) ExportCSV(ctx context.Context, q Query, o ExportOptions, w io.Writer) error {
	if o.Kind != "" && o.Kind != "5h" && o.Kind != "weekly" {
		return ErrInput
	}
	if len(o.ProfileReferences) > 100 {
		return ErrInput
	}
	c := csv.NewWriter(w)
	if o.WindowID != "" || q.EvidencePin != nil {
		p, e := r.Evidence(ctx, q, o.WindowID)
		if e != nil {
			return e
		}
		exports.WriteQuotaEvidence(c, p)
		c.Flush()
		return c.Error()
	}
	if o.CSV != "daily" && o.CSV != "comparisons" {
		return ErrInput
	}
	p, e := r.Report(ctx, q)
	if e != nil {
		return e
	}
	if o.CSV == "comparisons" {
		selected := ""
		for _, ref := range o.ProfileReferences {
			found := false
			for _, profile := range p.Profiles {
				if "profile:"+hash(profile.ID) == ref {
					if selected != "" {
						selected += ","
					}
					selected += profile.ID
					found = true
					break
				}
			}
			if !found {
				return ErrInput
			}
		}
		exports.WriteComparisons(c, p, o.Kind, selected)
	} else {
		_ = c.Write([]string{"date_utc", "requests", "tokens", "captured_cost_nano_usd", "captured_unpriced_requests", "api_value_low_nano_usd", "api_value_high_nano_usd", "missing_price_requests", "invalid_counter_requests", "valuation_rule", "api_value_json", "filter_json"})
		i := func(v int64) string { return strconv.FormatInt(v, 10) }
		for _, d := range p.Timeline {
			value, _ := json.Marshal(d.Value)
			filter, _ := json.Marshal(p.Filter)
			_ = c.Write([]string{d.Key, i(d.Requests), i(d.Tokens.Total), i(d.CostNano), i(d.Unpriced), i(d.Value.Low), i(d.Value.High), i(d.Value.Missing), i(d.Value.Invalid), p.PriceBook.ID, string(value), string(filter)})
		}
	}
	c.Flush()
	return c.Error()
}
