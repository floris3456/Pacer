package pluginapi

import (
	"context"
	"encoding/json"
)

// CodexOriginProvider binds native Responses state to completed physical Codex
// attempts. The host supplies caller and account identity; clients cannot call it.
type CodexOriginProvider interface {
	CodexOrigin(context.Context, CodexOriginRequest) error
}

type CodexOriginRequest struct {
	Version      int             `json:"version"`
	Operation    string          `json:"operation"` // validate | observe
	RequestID    string          `json:"request_id"`
	CallerScope  string          `json:"caller_scope"`
	AuthID       string          `json:"auth_id"`
	Identity     string          `json:"identity"`
	Body         json.RawMessage `json:"body"`
	OriginalBody json.RawMessage `json:"original_body,omitempty"`
}

type CodexOriginCapability struct {
	Version       int  `json:"version"`
	PhysicalHTTP  bool `json:"physical_http"`
	NativeCompact bool `json:"native_compact"`
}
