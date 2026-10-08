package pluginapi

import "time"

type HostAuthInventoryCheckpointRequest struct {
	Provider string `json:"provider"`
}

// HostAuthInventoryCheckpointResponse lists the host's credential tuples for one
// provider at one inventory sequence. Coverage is known, unknown or unsupported.
type HostAuthInventoryCheckpointResponse struct {
	Version              int                      `json:"version"`
	Provider             string                   `json:"provider"`
	Coverage             string                   `json:"coverage"`
	Reason               string                   `json:"reason,omitempty"`
	ManagerIncarnationID string                   `json:"manager_incarnation_id"`
	Sequence             uint64                   `json:"sequence"`
	At                   time.Time                `json:"at"`
	Tuples               []HostAuthInventoryTuple `json:"tuples"`
}

// HostAuthInventoryTuple is one credential tuple. An absent AccountRef means the
// billing account is unknown. A binding change is a tuple added or removed or an
// account_ref change; a token refresh for the same account is not one.
type HostAuthInventoryTuple struct {
	AuthID          string `json:"auth_id"`
	AuthIndex       string `json:"auth_index"`
	AccountRef      string `json:"account_ref,omitempty"`
	BindingSequence uint64 `json:"binding_sequence"`
	State           string `json:"state"`
}
