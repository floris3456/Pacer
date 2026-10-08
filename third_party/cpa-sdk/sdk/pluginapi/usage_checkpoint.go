package pluginapi

import "time"

const ClaudeHTTPUsageSource = "cpa.claude.http.first-party"
const ClaudeHTTPUsageSourceVersion = 1

type UsageSourceScope struct {
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Kind      string `json:"kind"`
	Version   int    `json:"version"`
}

type UsageSourceEvidence struct {
	Scope                UsageSourceScope  `json:"scope"`
	ManagerIncarnationID string            `json:"manager_incarnation_id"`
	Tracked              bool              `json:"tracked"`
	RequestedAt          time.Time         `json:"requested_at"`
	UpstreamStartedAt    *time.Time        `json:"upstream_started_at,omitempty"`
	FinalUsageAt         time.Time         `json:"final_usage_at"`
	Gap                  *UsageDeliveryGap `json:"gap,omitempty"`
}

type UsageDeliveryGap struct {
	Sequence   uint64     `json:"sequence"`
	Reason     string     `json:"reason"`
	From       *time.Time `json:"from,omitempty"`
	Through    *time.Time `json:"through,omitempty"`
	ObservedAt time.Time  `json:"observed_at"`
}

type HostUsageCheckpointRequest struct {
	Scope UsageSourceScope `json:"scope"`
}
type HostUsageCheckpointResponse struct {
	Scope                UsageSourceScope   `json:"scope"`
	Coverage             string             `json:"coverage"`
	Reason               string             `json:"reason,omitempty"`
	ManagerIncarnationID string             `json:"manager_incarnation_id"`
	IncarnationID        string             `json:"incarnation_id,omitempty"`
	Sequence             uint64             `json:"sequence"`
	From                 *time.Time         `json:"from,omitempty"`
	Through              *time.Time         `json:"through,omitempty"`
	Active               uint64             `json:"active"`
	Pending              uint64             `json:"pending"`
	GapCount             uint64             `json:"gap_count"`
	InvalidFrom          *time.Time         `json:"invalid_from,omitempty"`
	Gaps                 []UsageDeliveryGap `json:"gaps"`
	GapsTruncated        bool               `json:"gaps_truncated"`
}
