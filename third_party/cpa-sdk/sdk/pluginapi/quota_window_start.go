package pluginapi

import "net/http"

// QuotaWindowStartCapability describes the narrow, host-owned Claude starter.
type QuotaWindowStartCapability struct {
	Version       int  `json:"version"`
	Claude        bool `json:"claude"`
	ExactAccount  bool `json:"exact_account"`
	SingleAttempt bool `json:"single_attempt"`
	NoCache       bool `json:"no_cache"`
}

// HostQuotaWindowStartRequest consumes one host.http.operation_open operation.
// Only the current native Pacer instance may call host.claude.quota_window_start.
// AccountKey is "claude:" + hex(SHA256("claude:account:" + account_uuid)).
type HostQuotaWindowStartRequest struct {
	Version     int    `json:"version"`
	AuthID      string `json:"auth_id"`
	AccountKey  string `json:"account_key"`
	Model       string `json:"model"`
	OperationID string `json:"operation_id"`
}

// QuotaWindowStartRequestID correlates the one-use host operation with physical
// usage. Pacer captures the selected credential under this ID before dispatch;
// it is not a user session or a cache-policy enrollment. Host operation IDs are
// opaque and include a random host incarnation to prevent reuse across restarts.
func QuotaWindowStartRequestID(operationID string) string {
	return "pacer-window-start:" + operationID
}

// HostQuotaWindowStartResponse never includes generated content or token usage.
// Dispatched is conservative: true includes transport failures where upstream
// receipt is unknown. Only a successful RPC response with false proves no send.
type HostQuotaWindowStartResponse struct {
	Version    int         `json:"version"`
	Dispatched bool        `json:"dispatched"`
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	Reason     string      `json:"reason,omitempty"`
}

const (
	QuotaWindowStartInvalidRequest     = "invalid_request"
	QuotaWindowStartUnsupportedRuntime = "unsupported_runtime"
	QuotaWindowStartCredentialChanged  = "credential_unavailable"
	QuotaWindowStartCancelled          = "cancelled"
	QuotaWindowStartUpstreamError      = "upstream_error"
	QuotaWindowStartResponseIncomplete = "response_incomplete"
)

// QuotaWindowStartModelAllowed deliberately accepts complete native model IDs,
// never a routing alias, thinking suffix, or model-pool expression.
func QuotaWindowStartModelAllowed(model string) bool {
	switch model {
	case "claude-haiku-5-5", "claude-haiku-4-5", "claude-haiku-4-5-20251001",
		"claude-3-5-haiku-latest", "claude-3-5-haiku-20241022", "claude-3-haiku-20240307":
		return true
	default:
		return false
	}
}
