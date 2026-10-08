package historyreports

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/exports"
)

type LegacyExportOptions struct {
	View     string `json:"view"` // profiles, profile-proof, history, history-proof
	WindowID string `json:"window,omitempty"`
	Kind     string `json:"kind,omitempty"` // raw history list only
}

func (r *Reader) legacyExport(ctx context.Context, q LegacyQuery, o LegacyExportOptions) (any, error) {
	if o.View != "history" && o.Kind != "" {
		return nil, ErrInput
	}
	if o.View == "profiles" || o.View == "history" {
		if o.WindowID != "" {
			return nil, ErrInput
		}
	}
	switch o.View {
	case "profiles":
		return r.ProfileReport(ctx, q)
	case "profile-proof":
		return r.ProfileProof(ctx, q, o.WindowID)
	case "history":
		return r.HistoryReport(ctx, q, o.Kind)
	case "history-proof":
		return r.HistoryProof(ctx, q, o.WindowID)
	default:
		return nil, ErrInput
	}
}
func (r *Reader) ExportLegacyJSON(ctx context.Context, q LegacyQuery, o LegacyExportOptions, w io.Writer) error {
	value, e := r.legacyExport(ctx, q, o)
	if e != nil {
		return e
	}
	return json.NewEncoder(w).Encode(value)
}
func (r *Reader) ExportLegacyCSV(ctx context.Context, q LegacyQuery, o LegacyExportOptions, w io.Writer) error {
	value, e := r.legacyExport(ctx, q, o)
	if e != nil {
		return e
	}
	c := csv.NewWriter(w)
	switch v := value.(type) {
	case LegacyProfileReport:
		exports.WriteLegacyProfileReport(c, v)
	case LegacyProfileProof:
		exports.WriteLegacyProfileProof(c, v)
	case LegacyHistoryReport:
		exports.WriteHistoryReport(c, v)
	case LegacyHistoryProof:
		exports.WriteHistoryProof(c, v)
	default:
		return ErrInput
	}
	c.Flush()
	return c.Error()
}
func (r *Reader) ExportQuotaJSON(ctx context.Context, q QuotaQuery, w io.Writer) error {
	value, e := r.QuotaUsage(ctx, q)
	if e != nil {
		return e
	}
	return json.NewEncoder(w).Encode(value)
}
func (r *Reader) ExportQuotaCSV(ctx context.Context, q QuotaQuery, w io.Writer) error {
	value, e := r.QuotaUsage(ctx, q)
	if e != nil {
		return e
	}
	c := csv.NewWriter(w)
	exports.WriteQuotaUsage(c, value, value.Filter)
	c.Flush()
	return c.Error()
}
