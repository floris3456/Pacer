package pacer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The registration capability comes only from the trusted management.register
// RPC. It is revocable and bound to this configured publisher, never an HTTP body.
type forwardManagementBinding struct {
	registration pluginapi.ManagementRegistrationRequest
	publisher    *forwardPublisher
	ctx          context.Context
	cancel       context.CancelFunc
}

func (p *Plugin) revokeForwardManagementLocked() {
	p.forwardManagementEpoch++
	if old := p.forwardManagement; old != nil {
		old.cancel()
	}
	p.forwardManagement = nil
}

func (p *Plugin) revokeForwardManagement() {
	p.mu.Lock()
	p.revokeForwardManagementLocked()
	p.mu.Unlock()
}

func (p *Plugin) registerForwardManagement(raw []byte) []map[string]string {
	// Serializes registration, not analysis/SQL or the publisher. Any attempted
	// re-registration first revokes old authority, even an absent/malformed one.
	p.forwardManagementMu.Lock()
	defer p.forwardManagementMu.Unlock()
	p.mu.Lock()
	p.revokeForwardManagementLocked()
	epoch, w := p.forwardManagementEpoch, p.forward
	p.mu.Unlock()
	var reg pluginapi.ManagementRegistrationRequest
	if len(raw) > forwardHTTPInputLimit || forwardRegistrationDecode(raw, &reg) != nil || len(forwardV8Routes(reg, true)) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if epoch != p.forwardManagementEpoch || w == nil || w != p.forward || p.db == nil || p.stopped || w.ctx.Err() != nil || p.cfg.ForwardReportPath == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(w.ctx)
	p.forwardManagement = &forwardManagementBinding{registration: reg, publisher: w, ctx: ctx, cancel: cancel}
	return forwardV8Routes(reg, true)
}

// Validate the actual SDK shape with exact-case names, duplicates at every depth,
// bounded typed scanning and its normal JSON decoding. Nil metadata slices are
// valid in native SDK serialization; the capability scalar cannot be null.
func forwardRegistrationDecode(raw []byte, reg *pluginapi.ManagementRegistrationRequest) error {
	// The shared strict scanner uses JSON tags. These aliases add tags only at the
	// SDK's untagged struct fields; final decoding is into the actual SDK type.
	var wire struct {
		Plugin struct {
			Name             string     `json:"Name"`
			Version          string     `json:"Version"`
			Author           string     `json:"Author"`
			GitHubRepository string     `json:"GitHubRepository"`
			Logo             string     `json:"Logo"`
			ConfigFields     []struct{} `json:"ConfigFields"`
		} `json:"Plugin"`
		BasePath                 string `json:"BasePath"`
		ResourceBasePath         string `json:"ResourceBasePath"`
		PluginManagementBasePath string `json:"PluginManagementBasePath,omitempty"`
		HostCallbackID           string `json:"host_callback_id,omitempty"`
	}
	// Pacer declares no ConfigFields in its registration metadata. Nonempty host
	// extensions fail closed for forward only; legacy registration is preserved.
	if err := forwardHTTPDecode(raw, &wire, true); err != nil || len(wire.Plugin.ConfigFields) != 0 {
		return errForwardHTTPInput
	}
	return json.Unmarshal(raw, reg)
}

// Detect *any* v8 Path, including duplicate/case-alias declarations, before the
// legacy decoder can accept last-wins JSON. This only selects the strict path;
// it grants no authority. Legacy-only native envelopes retain their old decoder.
func forwardManagementRPC(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return false
	}
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return false
		}
		key, ok := t.(string)
		if !ok {
			return false
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return false
		}
		if strings.EqualFold(key, "Path") {
			var path string
			if json.Unmarshal(value, &path) == nil && strings.HasPrefix(path, "/v8") {
				return true
			}
		}
	}
	return false
}

// WithForwardStore leases the owner's store without taking a report slot. The
// HTTP adapter takes the ONE shared admission inside the callback. The existing
// WithForwardReports API remains for callers that also need automatic admission.
// fn must honor cancellation, never retain/close s, and finish before returning.
func (p *Plugin) WithForwardStore(ctx context.Context, fn func(context.Context, *historyreports.ForwardStore) error) error {
	if ctx == nil || fn == nil {
		return ErrForwardUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	w, stopped := p.forward, p.stopped
	p.mu.Unlock()
	if w == nil || stopped {
		return ErrForwardUnavailable
	}
	w.life.Lock()
	if w.stopped {
		w.life.Unlock()
		return ErrForwardUnavailable
	}
	w.wg.Add(1)
	w.life.Unlock()
	defer w.wg.Done()
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	unhook := context.AfterFunc(w.ctx, cancel)
	defer unhook()
	if w.ctx.Err() != nil {
		cancel()
	}
	if err := c.Err(); err != nil {
		return err
	}
	return fn(c, w.store)
}

func (p *Plugin) handleForwardManagement(ctx context.Context, raw []byte) pluginapi.ManagementResponse {
	p.mu.Lock()
	binding, stopped := p.forwardManagement, p.stopped
	p.mu.Unlock()
	if stopped {
		return forwardHTTPFailure(503, "store_unavailable")
	}
	if binding == nil {
		return forwardHTTPFailure(404, "not_found")
	}
	if ctx == nil {
		return forwardHTTPFailure(503, "request_cancelled")
	}
	c, cancel := context.WithTimeout(ctx, forwardHTTPDeadline)
	defer cancel()
	unhook := context.AfterFunc(binding.ctx, cancel)
	defer unhook()
	if binding.ctx.Err() != nil {
		cancel()
	}
	var response pluginapi.ManagementResponse
	err := p.WithForwardStore(c, func(leaseCtx context.Context, store *historyreports.ForwardStore) error {
		p.mu.Lock()
		valid := p.forwardManagement == binding && p.forward == binding.publisher && !p.stopped
		p.mu.Unlock()
		if !valid {
			return ErrForwardUnavailable
		}
		if p.forwardManagementLeaseHook != nil {
			p.forwardManagementLeaseHook(leaseCtx)
		}
		var snapshot ForwardRuntimeStatus
		adapter := NewForwardHTTP(store, p.reportSlots, ForwardHTTPOptions{
			Registration: binding.registration, Enabled: true,
			Status: func(ctx context.Context) (ForwardHTTPRuntimeStatus, error) {
				if err := ctx.Err(); err != nil {
					return ForwardHTTPRuntimeStatus{}, err
				}
				var err error
				snapshot, err = forwardPublisherHTTPStatus(binding.publisher)
				if err != nil {
					return ForwardHTTPRuntimeStatus{}, err
				}
				state := snapshot.State
				switch state {
				case "initializing":
					state = "unready"
				case "lag":
					state = "lagging"
				case "stopped":
					state = "unready"
				}
				return ForwardHTTPRuntimeStatus{State: state, TargetThrough: &snapshot.SourceHead}, nil
			},
			// The owner's channel is open for its entire lifetime, coalesced and
			// sent only after committed CAS. No relay goroutine or new pool.
			Notify: binding.publisher.wake,
		})
		var err error
		response, err = adapter.HandleRPC(leaseCtx, pluginabi.MethodManagementHandle, raw)
		if err != nil {
			return err
		}
		if snapshot.State != "" && response.StatusCode == 200 {
			var status forwardWiredStatus
			if err = json.Unmarshal(response.Body, &status); err != nil {
				return err
			}
			status.Publisher = snapshot
			response = forwardHTTPJSON(200, status)
		}
		// A committed profile CAS must stay 202/pending even if revoked while
		// notifying. Reads must not return a false 200 after shutdown/revocation.
		if response.StatusCode != 202 {
			if err = leaseCtx.Err(); err != nil {
				return err
			}
		}
		response = forwardNativeResponseBound(response)
		if response.StatusCode != 202 {
			if err = leaseCtx.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return forwardHTTPError(err)
	}
	return response
}

// Store capture and publisher metadata are explicitly separate, non-atomic
// snapshots. Only publisher.Latest/ReportAt name its last sealed publication.
type forwardWiredStatus struct {
	Version          string                   `json:"version"`
	State            string                   `json:"state"`
	FeedID           string                   `json:"feed_id"`
	ProjectedThrough int64                    `json:"projected_through,string"`
	BootstrapThrough int64                    `json:"bootstrap_through,string"`
	Latest           string                   `json:"latest_generation"`
	Runtime          ForwardHTTPRuntimeStatus `json:"runtime"`
	Publisher        ForwardRuntimeStatus     `json:"publisher"`
}

func forwardPublisherHTTPStatus(w *forwardPublisher) (ForwardRuntimeStatus, error) {
	s, _ := w.snapshot()
	switch s.State {
	case "disabled", "initializing", "lag", "error", "ready", "stopped":
	default:
		return ForwardRuntimeStatus{}, ErrForwardUnavailable
	}
	switch s.Phase {
	case "", "bootstrap", "projection", "provenance_feed", "provenance", "seal":
	default:
		return ForwardRuntimeStatus{}, ErrForwardUnavailable
	}
	switch s.ErrorCode {
	case "", "forward_bootstrap_failed", "forward_source_unavailable", "forward_selection_unavailable", "forward_store_unavailable", "forward_projection_failed", "forward_provenance_feed_failed", "forward_compute_unavailable", "forward_seal_failed", "forward_phase_invalid":
	default:
		return ForwardRuntimeStatus{}, ErrForwardUnavailable
	}
	if s.SourceHead < 0 || s.Projected < 0 || s.Latest != "" && !forwardHTTPHash(s.Latest) || s.Capacity != "unavailable" {
		return ForwardRuntimeStatus{}, ErrForwardUnavailable
	}
	return s, nil
}

// Bound the complete native C ABI envelope as well as the HTTP JSON body: SDK
// []byte adds base64 expansion around the already-encoded download envelope.
func forwardNativeResponseBound(response pluginapi.ManagementResponse) pluginapi.ManagementResponse {
	result, err := json.Marshal(response)
	if err != nil {
		return forwardHTTPError(err)
	}
	encoded, err := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
	if err != nil {
		return forwardHTTPError(err)
	}
	if len(encoded) > forwardHTTPOutputLimit {
		return forwardHTTPFailure(413, "response_too_large")
	}
	return response
}
