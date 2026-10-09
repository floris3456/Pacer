package pacer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Usage Quota page routes. Reads come from prepared quota facts held in memory
// and never take one of the two report-computation slots; exclusions are small
// revisioned records in the forward store.
const usageRoot = "/v0/management/plugins/pacer/usage/"

func usageRoutes() []map[string]string {
	return []map[string]string{
		{"Method": "POST", "Path": "/plugins/pacer/usage/profiles"},
		{"Method": "POST", "Path": "/plugins/pacer/usage/windows"},
		{"Method": "POST", "Path": "/plugins/pacer/usage/inspect"},
		{"Method": "PATCH", "Path": "/plugins/pacer/usage/exclusions"},
		{"Method": "POST", "Path": "/plugins/pacer/usage/accounts"},
		{"Method": "PATCH", "Path": "/plugins/pacer/usage/links"},
		{"Method": "PATCH", "Path": "/plugins/pacer/usage/profile"},
		{"Method": "POST", "Path": "/plugins/pacer/models/prices"},
		{"Method": "PATCH", "Path": "/plugins/pacer/models/prices"},
		{"Method": "POST", "Path": "/plugins/pacer/models/prices/refresh"},
	}
}

type usageExclusionEdit struct {
	Revision int64  `json:"revision"`
	Profile  string `json:"profile"`
	Window   string `json:"window"`
	Excluded bool   `json:"excluded"`
	Note     string `json:"note,omitempty"`
}

var errUsageExclusionsUnavailable = errors.New("usage exclusions unavailable")

// withForwardStore leases the forward store for a short metadata operation,
// without the report-computation slots.
func (p *Plugin) withForwardStore(ctx context.Context, fn func(context.Context, *historyreports.ForwardStore) error) error {
	p.mu.Lock()
	w := p.forward
	p.mu.Unlock()
	if w == nil {
		return errUsageExclusionsUnavailable
	}
	w.life.Lock()
	if w.stopped {
		w.life.Unlock()
		return errUsageExclusionsUnavailable
	}
	w.wg.Add(1)
	w.life.Unlock()
	defer w.wg.Done()
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(w.ctx, cancel)()
	return fn(c, w.store)
}

// usageExclusions reads the current exclusion set; without a forward store the
// set is empty and not editable.
func (p *Plugin) usageExclusions(ctx context.Context) (historyreports.WindowExclusions, bool, error) {
	var out historyreports.WindowExclusions
	err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
		var e error
		out, e = s.WindowExclusions(c)
		return e
	})
	if errors.Is(err, errUsageExclusionsUnavailable) {
		return historyreports.WindowExclusions{Excluded: []historyreports.WindowExclusion{}}, false, nil
	}
	return out, err == nil, err
}

func (p *Plugin) usageAPI(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	op := strings.TrimPrefix(req.Path, usageRoot)
	if op == "accounts" || op == "links" || op == "profile" {
		trimmed := bytes.TrimSpace(req.Body)
		if len(req.Body) > 64<<10 || len(req.Query) != 0 || len(trimmed) == 0 || trimmed[0] != '{' {
			return reportError(historyreports.ErrInput)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return p.usageSettingsAPI(ctx, op, req)
	}
	read := req.Method == "POST" && (op == "profiles" || op == "windows" || op == "inspect")
	edit := req.Method == "PATCH" && op == "exclusions"
	if !read && !edit {
		return resource(404, map[string]string{"error": "not_found"})
	}
	if len(req.Body) > 64<<10 {
		return reportError(errReportSize)
	}
	trimmed := bytes.TrimSpace(req.Body)
	if len(req.Query) != 0 || len(trimmed) == 0 || trimmed[0] != '{' {
		return reportError(historyreports.ErrInput)
	}
	var profiles historyreports.CompareQuery
	var windows historyreports.CompareWindowsQuery
	var inspect historyreports.InspectQuery
	var change usageExclusionEdit
	var input any
	switch op {
	case "profiles":
		input = &profiles
	case "windows":
		input = &windows
	case "inspect":
		input = &inspect
	default:
		input = &change
	}
	if decodeStrictReport(req.Body, input) != nil {
		return reportError(historyreports.ErrInput)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if edit {
		var out historyreports.WindowExclusions
		err := p.withForwardStore(ctx, func(c context.Context, s *historyreports.ForwardStore) error {
			var e error
			out, e = s.SetWindowExclusion(c, change.Revision, change.Profile, change.Window, change.Excluded, change.Note, p.now())
			return e
		})
		switch {
		case err == nil:
			return boundedReportResponse(out)
		case errors.Is(err, errUsageExclusionsUnavailable):
			return resource(409, map[string]string{"error": "Window exclusions need Pacer's usage store; it is not configured."})
		case errors.Is(err, historyreports.ErrForwardConflict):
			return resource(409, map[string]string{"error": "Exclusions changed meanwhile. Reload and try again."})
		case errors.Is(err, historyreports.ErrInput):
			return resource(400, map[string]string{"error": "Invalid profile, window or note"})
		default:
			return resource(503, map[string]string{"error": "The exclusion could not be saved"})
		}
	}
	p.mu.Lock()
	reader := p.reports
	p.mu.Unlock()
	if reader == nil {
		return resource(409, map[string]string{"error": "No usage history is prepared yet."})
	}
	exclusions, editable, err := p.usageExclusions(ctx)
	if err != nil {
		return resource(503, map[string]string{"error": "Window exclusions are unavailable"})
	}
	ex := exclusions.Map()
	var value any
	switch op {
	case "profiles":
		var view historyreports.CompareView
		view, err = reader.Compare(ctx, profiles, ex)
		view.ExclusionsRevision, view.ExclusionsEditable = exclusions.Revision, editable
		view.Live = p.usageLiveStatus()
		value = view
	case "windows":
		value, err = reader.CompareWindows(ctx, windows, ex)
	case "inspect":
		value, err = reader.Inspect(ctx, inspect, ex)
	}
	if err != nil {
		return reportError(err)
	}
	return boundedReportResponse(value)
}
