package pacer

import (
	"context"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const legacyHistoryRoot = historyRoot + "legacy/"

func legacyReportRoutes() []map[string]string {
	out := []map[string]string{{"Method": "GET", "Path": "/plugins/pacer/history/legacy/status"}}
	for _, op := range []string{"profiles", "profile-proof", "history", "history-proof", "forecasts", "quota", "export", "quota-export"} {
		out = append(out, map[string]string{"Method": "POST", "Path": "/plugins/pacer/history/legacy/" + op})
	}
	return out
}

type legacyProfileRequest struct {
	Query historyreports.LegacyQuery `json:"query"`
}
type legacyProofRequest struct {
	Query    historyreports.LegacyQuery `json:"query"`
	WindowID string                     `json:"window_id"`
}
type legacyHistoryRequest struct {
	Query historyreports.LegacyQuery `json:"query"`
	Kind  string                     `json:"kind,omitempty"`
}
type legacyForecastRequest struct {
	Query    historyreports.LegacyQuery `json:"query"`
	WindowID string                     `json:"window_id"`
	Limit    int                        `json:"limit,omitempty"`
	Cursor   string                     `json:"cursor,omitempty"`
}
type legacyExportRequest struct {
	Query    historyreports.LegacyQuery `json:"query"`
	View     string                     `json:"view"`
	WindowID string                     `json:"window_id,omitempty"`
	Kind     string                     `json:"kind,omitempty"`
	Format   string                     `json:"format"`
}
type legacyQuotaRequest struct {
	Query historyreports.QuotaQuery `json:"query"`
}
type legacyQuotaExportRequest struct {
	Query  historyreports.QuotaQuery `json:"query"`
	Format string                    `json:"format"`
}

// Dispatch is deliberately exact and management-only. Domain filters never
// accept a file, credential, preparation option or mutable source profile.
func (p *Plugin) legacyReportsAPI(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	op := strings.TrimPrefix(req.Path, legacyHistoryRoot)
	var input any
	if strings.HasPrefix(req.Path, legacyHistoryRoot) && req.Method == "POST" {
		switch op {
		case "profiles":
			input = &legacyProfileRequest{}
		case "profile-proof", "history-proof":
			input = &legacyProofRequest{}
		case "history":
			input = &legacyHistoryRequest{}
		case "forecasts":
			input = &legacyForecastRequest{}
		case "quota":
			input = &legacyQuotaRequest{}
		case "export":
			input = &legacyExportRequest{}
		case "quota-export":
			input = &legacyQuotaExportRequest{}
		}
	}
	status := req.Path == legacyHistoryRoot+"status" && req.Method == "GET"
	if input == nil && !status {
		return resource(404, map[string]string{"error": "not_found"})
	}
	if len(req.Body) > 64<<10 {
		return reportError(errReportSize)
	}
	if len(req.Query) != 0 || status && len(req.Body) != 0 {
		return reportError(historyreports.ErrInput)
	}
	if !status && decodeStrictReport(req.Body, input) != nil {
		return reportError(historyreports.ErrInput)
	}
	if f, ok := input.(*legacyForecastRequest); ok && len(f.Cursor) > 4096 {
		return reportError(historyreports.ErrInput)
	}
	return p.withReportReader(func(ctx context.Context, reader *historyreports.Reader) pluginapi.ManagementResponse {
		if status {
			pin := reader.DefaultPin().ProfileFilter
			pin.ValuationRule = ""
			return boundedReportResponse(map[string]any{"historical_ready": reader.HistoricalReady(), "default_pin": pin, "coverage": "Frozen checkpoint only. Saved forecasts are retained evidence, not a live worker. Scenario ratios are explicit assumptions; current native measurements are separate."})
		}
		if !reader.HistoricalReady() {
			return reportError(historyreports.ErrNotPrepared)
		}
		var value any
		var err error
		switch v := input.(type) {
		case *legacyProfileRequest:
			value, err = reader.ProfileReport(ctx, v.Query)
		case *legacyProofRequest:
			if op == "profile-proof" {
				value, err = reader.ProfileProof(ctx, v.Query, v.WindowID)
			} else {
				value, err = reader.HistoryProof(ctx, v.Query, v.WindowID)
			}
		case *legacyHistoryRequest:
			value, err = reader.HistoryReport(ctx, v.Query, v.Kind)
		case *legacyForecastRequest:
			value, err = reader.Forecasts(ctx, historyreports.ForecastQuery{Selection: v.Query, WindowID: v.WindowID, Limit: v.Limit, Cursor: v.Cursor})
		case *legacyQuotaRequest:
			value, err = reader.QuotaUsage(ctx, v.Query)
		case *legacyExportRequest:
			o := historyreports.LegacyExportOptions{View: v.View, WindowID: v.WindowID, Kind: v.Kind}
			var output boundedReport
			switch v.Format {
			case "json":
				err = reader.ExportLegacyJSON(ctx, v.Query, o, &output)
			case "csv":
				err = reader.ExportLegacyCSV(ctx, v.Query, o, &output)
			default:
				err = historyreports.ErrInput
			}
			value = legacyDownload(v.View, v.Format, output.String())
		case *legacyQuotaExportRequest:
			var output boundedReport
			switch v.Format {
			case "json":
				err = reader.ExportQuotaJSON(ctx, v.Query, &output)
			case "csv":
				err = reader.ExportQuotaCSV(ctx, v.Query, &output)
			default:
				err = historyreports.ErrInput
			}
			value = legacyDownload("quota", v.Format, output.String())
		}
		if err != nil {
			return reportError(err)
		}
		return boundedReportResponse(value)
	})
}

func legacyDownload(view, format, content string) map[string]string {
	media := "application/json"
	if format == "csv" {
		media = "text/csv"
	}
	return map[string]string{"filename": "pacer-legacy-" + view + "." + format, "media_type": media, "content": content}
}
