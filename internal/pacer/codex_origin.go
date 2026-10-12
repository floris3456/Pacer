package pacer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/origin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func originError(err error) error {
	status := http.StatusConflict
	if errors.Is(err, origin.ErrInvalid) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, origin.ErrUnavailable) {
		status = http.StatusServiceUnavailable
	}
	return pluginabi.NewError("pacer_checkpoint_origin_required", "Encrypted Responses state requires its observed authorized account; create a portable checkpoint before switching accounts", status)
}

func originRefusal(err error) pluginapi.RequestInterceptResponse {
	e := originError(err).(*pluginabi.Error)
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: e.StatusCode(), ResponseBody: body}
}

// beforeOrigin intersects ownership with authorization before scheduling. An
// absent/unavailable owner stays a one-account pool and cannot fall back.
func (p *Plugin) beforeOrigin(req pluginapi.RequestInterceptRequest, pool *pluginapi.AuthPool) (*pluginapi.AuthPool, pluginapi.RequestInterceptResponse) {
	if !originRequestFormat(req) || len(req.Body) == 0 {
		return pool, pluginapi.RequestInterceptResponse{}
	}
	opaque, err := origin.HasOpaqueRequest(req.Body)
	if err != nil {
		return pool, originRefusal(err)
	}
	if !opaque {
		return pool, pluginapi.RequestInterceptResponse{}
	}
	caller := text(req.Metadata, "caller_scope")
	p.mu.Lock()
	store := p.codexOrigins
	allowed := p.clientAllowedLocked(caller, "")
	p.mu.Unlock()
	if !allowed {
		return pool, denyClientAccess()
	}
	owner, err := store.Owner(context.Background(), caller, req.Body)
	if err != nil {
		return pool, originRefusal(err)
	}
	p.mu.Lock()
	allowed = p.clientAllowedLocked(caller, owner)
	p.mu.Unlock()
	if !allowed {
		return pool, denyClientAccess()
	}
	if pool != nil {
		found := false
		for _, id := range pool.AuthIDs {
			if id == owner {
				found = true
				break
			}
		}
		if !found {
			return pool, denyClientAccess()
		}
	}
	return &pluginapi.AuthPool{Version: 1, AuthIDs: []string{owner}}, pluginapi.RequestInterceptResponse{}
}

// afterOrigin prevents an opaque body from crossing a provider boundary, even
// when another request interceptor changes the model after the initial route.
func (p *Plugin) afterOrigin(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if !originRequestFormat(req) || len(req.Body) == 0 {
		return pluginapi.RequestInterceptResponse{}
	}
	opaque, err := origin.HasOpaqueRequest(req.Body)
	if err != nil {
		return originRefusal(err)
	}
	if !opaque {
		return pluginapi.RequestInterceptResponse{}
	}
	if req.ToFormat != "codex" {
		return originRefusal(origin.ErrMismatch)
	}
	caller := text(req.Metadata, "caller_scope")
	p.mu.Lock()
	store := p.codexOrigins
	p.mu.Unlock()
	owner, err := store.Owner(context.Background(), caller, req.Body)
	if err != nil {
		return originRefusal(err)
	}
	if owner != text(req.Metadata, "selected_auth_id") {
		return originRefusal(origin.ErrMismatch)
	}
	return pluginapi.RequestInterceptResponse{}
}

func originRequestFormat(req pluginapi.RequestInterceptRequest) bool {
	return req.SourceFormat == "openai-response" || req.SourceFormat == "codex" || req.ToFormat == "codex"
}

func (p *Plugin) codexOrigin(raw []byte) (any, error) {
	var req pluginapi.CodexOriginRequest
	if len(raw) > 2*origin.MaxBodyBytes+64<<10 || json.Unmarshal(raw, &req) != nil || req.Version != 1 || req.RequestID == "" {
		return nil, originError(origin.ErrInvalid)
	}
	p.mu.Lock()
	store := p.codexOrigins
	allowed := p.clientAllowedLocked(req.CallerScope, req.AuthID)
	p.mu.Unlock()
	if !allowed {
		return nil, pluginabi.NewError("pacer_account_access_denied", "Account access is not granted to this authenticated caller", 403)
	}
	binding := origin.Binding{Caller: req.CallerScope, AuthID: req.AuthID, Identity: req.Identity}
	var err error
	switch req.Operation {
	case "validate":
		if len(req.OriginalBody) > 0 {
			err = store.Check(context.Background(), binding, req.OriginalBody)
		}
		if err == nil {
			err = store.Check(context.Background(), binding, req.Body)
		}
	case "observe":
		err = store.Observe(context.Background(), binding, req.Body)
	default:
		err = origin.ErrInvalid
	}
	if err != nil {
		return nil, originError(err)
	}
	return struct{}{}, nil
}
