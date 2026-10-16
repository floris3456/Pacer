package pacer

import (
	"context"
	"errors"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *Plugin) editUsagePolling(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if p.observer == nil {
		return resource(503, map[string]string{"error": "Quota polling is unavailable"})
	}
	if req.Method != "PATCH" {
		return resource(404, map[string]string{"error": "not_found"})
	}
	var edit struct {
		Profile  string `json:"profile"`
		Seconds  int    `json:"seconds"`
		Revision int64  `json:"revision"`
	}
	if decodeStrictReport(req.Body, &edit) != nil {
		return reportError(historyreports.ErrInput)
	}
	view, err := p.usageAccountsView(ctx)
	if err != nil {
		return reportError(err)
	}
	found := false
	for _, prof := range view.Profiles {
		if prof.Reference == edit.Profile && !prof.Archived && observation.Endpoint(prof.Provider) != "" {
			found = true
		}
	}
	if !found {
		return reportError(historyreports.ErrInput)
	}
	err = p.observer.SetPolling(edit.Profile, edit.Seconds, edit.Revision)
	if errors.Is(err, observation.ErrPollingConflict) {
		return resource(409, map[string]string{"error": "Polling setting changed. Refresh and try again."})
	}
	if errors.Is(err, observation.ErrPollingInput) {
		return resource(400, map[string]string{"error": "Use a whole number of minutes between 15 and 1440."})
	}
	if err != nil {
		return reportError(err)
	}
	return resource(200, map[string]any{"seconds": edit.Seconds, "revision": edit.Revision + 1})
}

func (p *Plugin) annotatePolling(view *historyreports.CompareView) error {
	if p.observer == nil {
		return nil
	}
	settings, err := p.observer.PollingSettings()
	if err != nil {
		return err
	}
	now := p.now().UnixMilli()
	for i := range view.Profiles {
		prof := &view.Profiles[i]
		seconds := int(observation.IdleInterval / time.Second)
		if s, ok := settings[prof.Reference]; ok {
			seconds = s.Seconds
			prof.PollingRevision = s.Revision
		}
		if !prof.Archived && observation.Endpoint(prof.Provider) != "" {
			prof.PollingSeconds = seconds
		}
		for j := range prof.Current {
			meter := &prof.Current[j]
			meter.Stale = meter.LastObservedMS == 0 || now-meter.LastObservedMS > int64(seconds+60)*1000 || meter.ResetMS > 0 && meter.ResetMS < now
		}
	}
	return nil
}

// Resolve current profile memberships through the record's linked login IDs.
// Shared accounts use the shortest requested interval. Unassigned accounts keep
// the default schedule; inference/header observation does not depend on this UI.
func (p *Plugin) applyProfilePolling(creds []credential) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	settings, err := p.observer.PollingSettings()
	if err != nil {
		return err
	}
	if len(settings) == 0 {
		return nil
	}
	view, err := p.usageAccountsView(ctx)
	if err != nil {
		return err
	}
	ix := p.usageIndex()
	p.mu.Lock()
	source := p.measurementSource
	p.mu.Unlock()
	intervals := map[string]int{}
	now := p.now().UnixMilli()
	for _, c := range creds {
		seconds := 0
		owner := ix.owner[historyreports.ForwardAccountRef(source, c.Provider, c.Account)]
		for _, prof := range view.Profiles {
			if prof.Archived {
				continue
			}
			for _, member := range prof.Accounts {
				if member.Account != owner.Ref || member.FromMS > now || member.ToMS != 0 && member.ToMS <= now {
					continue
				}
				next := int(observation.IdleInterval / time.Second)
				if s, ok := settings[prof.Reference]; ok {
					next = s.Seconds
				}
				if seconds == 0 || next < seconds {
					seconds = next
				}
			}
		}
		if seconds == 0 {
			seconds = int(observation.IdleInterval / time.Second)
		}
		intervals[c.Account] = seconds
	}
	return p.observer.ApplyPolling(intervals)
}
