package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const historyRoot = "/v0/management/plugins/pacer/history/"
const maxHistoryOutput = 8 << 20

func reportRoutes() []map[string]string {
	out := []map[string]string{{"Method": "GET", "Path": "/plugins/pacer/history/profiles"}}
	for _, name := range []string{"report", "windows", "evidence", "export"} {
		out = append(out, map[string]string{"Method": "POST", "Path": "/plugins/pacer/history/" + name})
	}
	return out
}

type reportRequest struct {
	Query             historyreports.Query `json:"query"`
	Kind              string               `json:"kind,omitempty"`
	State             string               `json:"state,omitempty"`
	Limit             int                  `json:"limit,omitempty"`
	Cursor            string               `json:"cursor,omitempty"`
	WindowID          string               `json:"window_id,omitempty"`
	Format            string               `json:"format,omitempty"`
	CSV               string               `json:"csv,omitempty"`
	ProfileReferences []string             `json:"profile_references,omitempty"`
}

type boundedReport struct{ bytes.Buffer }

var errReportSize = errors.New("history response too large")

func (b *boundedReport) Write(p []byte) (int, error) {
	if len(p) > maxHistoryOutput-b.Len() {
		return 0, errReportSize
	}
	return b.Buffer.Write(p)
}

func reportError(err error) pluginapi.ManagementResponse {
	status, message := 503, "Frozen report evidence is unavailable"
	switch {
	case errors.Is(err, historyreports.ErrInput), errors.Is(err, historyreports.ErrBoundary):
		status, message = 400, "Invalid report filter or pin outside the frozen checkpoint"
	case errors.Is(err, historyreports.ErrCursor):
		status, message = 409, "Window cursor no longer matches this report or page"
	case errors.Is(err, errReportSize), errors.Is(err, historyreports.ErrLimit):
		status, message = 413, "Report exceeds the bounded response limit; narrow the selection"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = 504, "Report calculation exceeded its cooperative deadline"
	}
	return resource(status, map[string]string{"error": message})
}

func boundedReportResponse(value any) pluginapi.ManagementResponse {
	var b boundedReport
	if err := json.NewEncoder(&b).Encode(value); err != nil {
		return reportError(err)
	}
	r := resource(200, nil)
	r.Body = b.Bytes()
	return r
}

// Only exact CPA-authenticated management paths reach this function. Paths and
// preparation are operator-only configuration; no request can materialize a DB.
func (p *Plugin) reportsAPI(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	op := strings.TrimPrefix(req.Path, historyRoot)
	if !strings.HasPrefix(req.Path, historyRoot) || !(req.Method == "GET" && op == "profiles" || req.Method == "POST" && (op == "report" || op == "windows" || op == "evidence" || op == "export")) {
		return resource(404, map[string]string{"error": "not_found"})
	}
	if len(req.Body) > 64<<10 {
		return reportError(errReportSize)
	}
	var body reportRequest
	revision := int64(-1)
	if op == "profiles" {
		if len(req.Body) != 0 || len(req.Query) > 1 {
			return reportError(historyreports.ErrInput)
		}
		for key, values := range req.Query {
			if key != "revision" || len(values) != 1 {
				return reportError(historyreports.ErrInput)
			}
			var err error
			revision, err = strconv.ParseInt(values[0], 10, 64)
			if err != nil || revision < -1 {
				return reportError(historyreports.ErrInput)
			}
		}
	} else {
		if len(req.Query) != 0 {
			return reportError(historyreports.ErrInput)
		}
		trimmed := bytes.TrimSpace(req.Body)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return reportError(historyreports.ErrInput)
		}
		if decodeReport(req.Body, op, &body) != nil {
			return reportError(historyreports.ErrInput)
		}
		if len(body.Cursor) > 4096 || len(body.WindowID) > 512 || len(body.ProfileReferences) > 100 {
			return reportError(historyreports.ErrInput)
		}
		// Do not silently ignore parameters from another operation.
		if op != "windows" && (body.State != "" || body.Limit != 0 || body.Cursor != "") {
			return reportError(historyreports.ErrInput)
		}
		if op != "export" && (body.Format != "" || body.CSV != "" || len(body.ProfileReferences) > 0) {
			return reportError(historyreports.ErrInput)
		}
		if op != "export" && op != "evidence" && body.WindowID != "" {
			return reportError(historyreports.ErrInput)
		}
		if op != "windows" && op != "export" && body.Kind != "" {
			return reportError(historyreports.ErrInput)
		}
	}
	p.mu.Lock()
	reader, gate := p.reports, p.reportSlots
	p.mu.Unlock()
	if reader == nil {
		return resource(409, map[string]string{"error": "No frozen native report artifact is configured. Keep CPAMP available; prepare reports offline."})
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		r := resource(429, map[string]string{"error": "Two historical calculations are already running; retry later"})
		r.Headers.Set("Retry-After", "1")
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var value any
	var err error
	switch op {
	case "profiles":
		var profiles historyreports.ProfileList
		profiles, err = reader.Profiles(ctx, revision)
		value = map[string]any{"profiles": profiles.Profiles, "source_revision": profiles.Revision, "default_pin": reader.DefaultPin(), "coverage": "Frozen checkpoint only. Current native usage is separate; CPAMP retirement and unported legacy views remain unverified."}
	case "report":
		value, err = reader.Report(ctx, body.Query)
	case "windows":
		value, err = reader.ListWindows(ctx, historyreports.WindowQuery{Query: body.Query, Kind: body.Kind, State: body.State, Limit: body.Limit, Cursor: body.Cursor})
	case "evidence":
		value, err = reader.Evidence(ctx, body.Query, body.WindowID)
	case "export":
		o := historyreports.ExportOptions{WindowID: body.WindowID, CSV: body.CSV, Kind: body.Kind, ProfileReferences: body.ProfileReferences}
		var output boundedReport
		media := "application/json"
		switch body.Format {
		case "json":
			err = reader.ExportJSON(ctx, body.Query, o, &output)
		case "csv":
			media = "text/csv"
			err = reader.ExportCSV(ctx, body.Query, o, &output)
		default:
			err = historyreports.ErrInput
		}
		// A JSON envelope works identically through the credential-free iframe
		// bridge. Downloaded content is the exact domain writer output bytes.
		value = map[string]string{"filename": "pacer-history." + body.Format, "media_type": media, "content": output.String()}
	}
	if err != nil {
		return reportError(err)
	}
	return boundedReportResponse(value)
}
