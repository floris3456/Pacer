package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxHistoryOutput = 8 << 20

type boundedReport struct{ bytes.Buffer }

var errReportSize = errors.New("history response too large")

func (b *boundedReport) Write(p []byte) (int, error) {
	if len(p) > maxHistoryOutput-b.Len() {
		return 0, errReportSize
	}
	return b.Buffer.Write(p)
}

func reportError(err error) pluginapi.ManagementResponse {
	status, message := 503, "Usage history is unavailable"
	switch {
	case errors.Is(err, historyreports.ErrNotPrepared):
		status, message = 409, "The usage history must be prepared again (offline) for this view"
	case errors.Is(err, historyreports.ErrInput), errors.Is(err, historyreports.ErrBoundary):
		status, message = 400, "Invalid filter, profile or window"
	case errors.Is(err, historyreports.ErrCursor):
		status, message = 409, "The page cursor no longer matches; reload"
	case errors.Is(err, errReportSize), errors.Is(err, historyreports.ErrLimit):
		status, message = 413, "The result is too large; narrow the selection"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = 504, "The calculation took too long"
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
