package pacer

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The instant Compare routes read prepared quota facts held in memory. They do
// not take one of the two slots that bound full historical calculations, so a
// running legacy report never delays them.
func compareRoutes() []map[string]string {
	return []map[string]string{
		{"Method": "POST", "Path": "/plugins/pacer/history/compare"},
		{"Method": "POST", "Path": "/plugins/pacer/history/compare-windows"},
	}
}

func isCompareOp(op string) bool { return op == "compare" || op == "compare-windows" }

func (p *Plugin) compareAPI(req pluginapi.ManagementRequest, op string) pluginapi.ManagementResponse {
	if req.Method != "POST" {
		return resource(404, map[string]string{"error": "not_found"})
	}
	if len(req.Body) > 64<<10 {
		return reportError(errReportSize)
	}
	trimmed := bytes.TrimSpace(req.Body)
	if len(req.Query) != 0 || len(trimmed) == 0 || trimmed[0] != '{' {
		return reportError(historyreports.ErrInput)
	}
	var plain historyreports.CompareQuery
	var windows historyreports.CompareWindowsQuery
	var err error
	if op == "compare" {
		err = decodeStrictReport(req.Body, &plain)
	} else {
		err = decodeStrictReport(req.Body, &windows)
	}
	if err != nil || len(plain.Key) > 6600 || len(windows.Key) > 6600 || strings.ContainsAny(plain.Key+windows.Key, "\x00") {
		return reportError(historyreports.ErrInput)
	}
	p.mu.Lock()
	reader := p.reports
	p.mu.Unlock()
	if reader == nil {
		return resource(409, map[string]string{"error": "No frozen native report artifact is configured. Keep CPAMP available; prepare reports offline."})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var value any
	if op == "compare" {
		value, err = reader.Compare(ctx, plain)
	} else {
		value, err = reader.CompareWindows(ctx, windows)
	}
	if err != nil {
		return reportError(err)
	}
	return boundedReportResponse(value)
}
