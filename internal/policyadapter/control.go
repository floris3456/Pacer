package policyadapter

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func command(op pluginapi.CachePolicyOperation) cachepolicy.Command {
	c := cachepolicy.Command{Protocol: op.Protocol, Operation: op.Operation,
		Scope:        cachepolicy.Scope{InstallationID: op.InstallationID, SessionID: op.SessionID, ProviderRoute: op.ProviderRoute},
		ContextEpoch: op.ContextEpoch, ExpectedRevision: op.ExpectedRevision, TTL: cachepolicy.TTL(op.TTL), OperationID: op.OperationID,
		TransitionID: op.TransitionID, Enrollment: op.Enrollment, Checkpoint: op.Checkpoint}
	if op.CompactionStartedAt != nil {
		c.CompactionStartedAt = *op.CompactionStartedAt
	}
	if op.CheckpointCommittedAt != nil {
		c.CheckpointCommittedAt = *op.CheckpointCommittedAt
	}
	if r := op.CheckpointReceipt; r != nil {
		c.CheckpointReceipt = &cachepolicy.CheckpointReceipt{Version: r.Version, TransitionID: r.TransitionID, PreviousCheckpoint: r.PreviousCheckpoint, Checkpoint: r.Checkpoint, Committed: r.Committed}
	}
	return c
}

func (a *Adapter) HandleCachePolicy(ctx context.Context, req pluginapi.CachePolicyRequest) (pluginapi.CachePolicyResponse, error) {
	state, err := a.control(ctx, req)
	if err != nil {
		e := rpcError(err).(*pluginabi.Error)
		body, _ := json.Marshal(struct {
			Error any `json:"error"`
		}{struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{e.Code, e.Message}})
		return pluginapi.CachePolicyResponse{StatusCode: e.HTTPStatus, Body: body}, nil
	}
	body, err := json.Marshal(state)
	if err != nil || len(body) > cachepolicy.MaxCommandBytes {
		return pluginapi.CachePolicyResponse{}, reject(503, "unavailable", "Policy response could not be encoded within its bound.")
	}
	return pluginapi.CachePolicyResponse{StatusCode: http.StatusOK, Body: body}, nil
}

func (a *Adapter) control(ctx context.Context, req pluginapi.CachePolicyRequest) (cachepolicy.State, error) {
	auth, err := credentials(req.Scope, req.Capability)
	if err != nil {
		return cachepolicy.State{}, err
	}
	if err = a.route(req.Scope.ProviderRoute); err != nil {
		return cachepolicy.State{}, err
	}
	op := req.Operation
	if op.Protocol != cachepolicy.Protocol {
		return cachepolicy.State{}, reject(422, "unsupported", "Unsupported policy protocol.")
	}
	if op.InstallationID != req.Scope.InstallationID || op.SessionID != req.Scope.SessionID || op.ProviderRoute != req.Scope.ProviderRoute {
		return cachepolicy.State{}, reject(403, "scope_denied", "Operation scope does not match the authenticated host scope.")
	}
	switch req.Method {
	case http.MethodGet:
		read := pluginapi.CachePolicyOperation{Protocol: cachepolicy.Protocol, Operation: "read", InstallationID: op.InstallationID, SessionID: op.SessionID, ProviderRoute: op.ProviderRoute}
		if op != read {
			return cachepolicy.State{}, reject(400, "invalid", "GET accepts only the protocol-1 read scope.")
		}
		return a.store.Get(ctx, auth, scope(req.Scope))
	case http.MethodPost:
		evidence := cachepolicy.Evidence{}
		if op.Operation == "ensure" && op.Enrollment == "new_empty" && a.authority != nil {
			evidence.NewEmpty, err = a.authority(ctx, EmptyClaim{Scope: req.Scope, ContextEpoch: op.ContextEpoch, OperationID: op.OperationID})
			if err != nil {
				return cachepolicy.State{}, reject(503, "attestation_unavailable", "Trusted empty-session attestation is unavailable.")
			}
		}
		return a.store.Apply(ctx, auth, command(op), evidence)
	default:
		return cachepolicy.State{}, reject(405, "method_not_allowed", "Policy operations require GET or POST.")
	}
}
