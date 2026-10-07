package pacer

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *Plugin) quotaAPI(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	index := req.Query.Get("auth_index")
	if req.Method == "POST" {
		if len(req.Body) > 4096 {
			return resource(413, map[string]string{"error": "body_too_large"})
		}
		var body struct {
			Index string `json:"auth_index"`
		}
		d := json.NewDecoder(bytes.NewReader(req.Body))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
			return resource(400, map[string]string{"error": "invalid_quota_request"})
		}
		index = body.Index
	}
	if index == "" || len(index) > 512 {
		return resource(400, map[string]string{"error": "invalid_auth_index"})
	}
	p.mu.Lock()
	var selected credential
	for _, c := range p.creds {
		if c.Index == index {
			selected = c
			break
		}
	}
	p.mu.Unlock()
	if selected.ID == "" {
		return resource(404, map[string]string{"error": "quota_credential_unavailable"})
	}
	if req.Method == "POST" {
		if err := p.observer.Queue(selected.Account, observation.Endpoint(selected.Provider), observation.ReasonManual, time.Time{}); err != nil {
			return resource(503, map[string]string{"error": "quota_schedule_unavailable"})
		}
	}
	windows, err := p.observer.ReadWindows(selected.Account)
	if err != nil {
		return resource(503, map[string]string{"error": "quota_state_unavailable"})
	}
	if windows == nil {
		windows = []observation.Window{}
	}
	job, _, err := p.observer.Status(selected.Account)
	if err != nil {
		return resource(503, map[string]string{"error": "quota_schedule_unavailable"})
	}
	return resource(200, map[string]any{"provider": selected.Provider, "now": p.now(), "windows": windows, "reconciliation": map[string]any{"next": job.Next, "blocked": job.Blocked, "reason": job.Reason, "state": job.State}})
}
