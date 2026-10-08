package pacer

import (
	"context"
	"errors"

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
	return resource(200, map[string]any{"revision": data.Revision, "keys": data.Keys, "groups": data.Groups, "coverage": "Preserved key names and exact replacement lineage. This archive is not yet a verified replacement for CPAMP's Usage Quota calculations. Display grouping does not authorize keys or change quota pools."})
}
