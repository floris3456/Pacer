package pluginapi

import "encoding/json"

type ClaudeReplayCapability struct {
	Version          int  `json:"version"`
	FinalWire        bool `json:"final_wire"`
	ExactCredential  bool `json:"exact_credential"`
	RootKeepalive    bool `json:"root_keepalive"`
	NativeCompaction bool `json:"native_compaction"`
}

// HostClaudeReplayRequest is available only to the current required native Pacer
// plugin and an authenticated live HTTP callback. It never selects an account.
type HostClaudeReplayRequest struct {
	HostCallbackID string           `json:"host_callback_id"`
	Operation      string           `json:"operation"`
	Kind           string           `json:"kind"`
	Scope          CachePolicyScope `json:"scope"`
	ContextEpoch   string           `json:"context_epoch"`
	Revision       int64            `json:"revision"`
	Capability     string           `json:"capability"`
	Token          string           `json:"token,omitempty"`
	Request        json.RawMessage  `json:"request,omitempty"`
}

type ClaudeReplayLease struct {
	Version          int    `json:"version"`
	Kind             string `json:"kind"`
	Token            string `json:"token"`
	RequestID        string `json:"request_id"`
	RequestStartedMS int64  `json:"request_started_ms"`
	ExpiresMS        int64  `json:"expires_ms"`
	NotBeforeMS      int64  `json:"not_before_ms"`
	UntilMS          int64  `json:"until_ms"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
}

type HostClaudeReplayResponse struct {
	Version int    `json:"version"`
	State   string `json:"state"`
	// Settled confirms transport closure and durable attempt settlement, even
	// when the operation did not produce a reusable cache or valid checkpoint.
	Settled          bool               `json:"settled"`
	Reason           string             `json:"reason,omitempty"`
	Lease            *ClaudeReplayLease `json:"lease,omitempty"`
	Response         json.RawMessage    `json:"response,omitempty"`
	CacheReadTokens  int64              `json:"cache_read_tokens"`
	CacheWriteTokens int64              `json:"cache_write_tokens"`
}
