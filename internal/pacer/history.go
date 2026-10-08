package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (p *Plugin) keysView() pluginapi.ManagementResponse {
	p.mu.Lock()
	store := p.history
	p.mu.Unlock()
	if store == nil {
		return resource(200, map[string]any{"revision": 0, "keys": []any{}, "groups": []any{}, "coverage": "No Pacer history archive is configured. CPAMP remains the source of key aliases, lineage and dated account profiles."})
	}
	data, err := store.Keys(context.Background())
	if err != nil {
		if errors.Is(err, history.ErrPending) {
			return resource(409, map[string]string{"error": "Historical import is incomplete. Continue the offline import; no partial metadata is published."})
		}
		return resource(503, map[string]string{"error": "Historical metadata is unavailable"})
	}
	return resource(200, map[string]any{"revision": data.Revision, "keys": data.Keys, "groups": data.Groups, "editable": true, "coverage": "Preserved key names and exact replacement lineage. This archive is not yet a verified replacement for CPAMP's Usage Quota calculations. Display grouping does not authorize keys or change quota pools."})
}

func (p *Plugin) editKeys(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) > 32<<10 {
		return resource(413, map[string]string{"error": "Metadata edit is too large"})
	}
	var edit history.Edit
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&edit) != nil || decoder.Decode(new(any)) != io.EOF {
		return resource(400, map[string]string{"error": "Invalid metadata edit"})
	}
	p.mu.Lock()
	store := p.history
	p.mu.Unlock()
	if store == nil {
		return resource(409, map[string]string{"error": "Configure a Pacer-owned history archive before editing metadata"})
	}
	_, err := store.Edit(context.Background(), edit)
	if err != nil {
		switch {
		case errors.Is(err, history.ErrConflict):
			return resource(409, map[string]string{"error": "Metadata changed or the requested name or lineage conflicts. Refresh and retry."})
		case errors.Is(err, history.ErrInput):
			return resource(400, map[string]string{"error": "Invalid display name, reference, or membership"})
		case errors.Is(err, history.ErrPending):
			return resource(409, map[string]string{"error": "Historical import is not yet published"})
		default:
			return resource(503, map[string]string{"error": "Metadata could not be saved"})
		}
	}
	return p.keysView()
}
