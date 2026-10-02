// Package bridge is the only code that knows the installed CPA ABI.
package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const ControlHeader = "X-Quota-Handoff"
const AckHeader = "X-Quota-Binding"

type Host func(string, any) (json.RawMessage, error)
type tracked struct {
	Estimate adaptive.Request
	Started  int64
	Caller   string
	Claim    *handoff.Claim
	Selected handoff.Flight
	Legacy   *legacyStream
}
type Plugin struct {
	usageAuthURL       string
	notificationsMuted atomic.Bool
	noticeLogMu        sync.Mutex
	loggedNotices      map[string]bool
	exchangeMu         sync.Mutex
	nextExchange       time.Time
	stopped            atomic.Bool
	closeOnce          sync.Once
	configDigest       string
	mu                 sync.Mutex
	Engine             *handoff.Engine
	Host               Host
	requests           map[string]tracked
	refreshMu          sync.Mutex
	next               map[string]time.Time
}

func New(host Host) *Plugin {
	return &Plugin{Host: host, requests: map[string]tracked{}, next: map[string]time.Time{}}
}
func (p *Plugin) Close() {
	p.closeOnce.Do(func() {
		p.stopped.Store(true)
		p.refreshMu.Lock()
		defer p.refreshMu.Unlock()
		p.exchangeMu.Lock()
		defer p.exchangeMu.Unlock()
		if p.Engine != nil {
			_ = p.Engine.Close()
		}
	})
}
func registration() any {
	return map[string]any{"schema_version": 6, "metadata": pluginapi.Metadata{Name: "quota-handoff", Version: "0.1.3-local.1", Author: "floris3456", GitHubRepository: "https://github.com/floris3456/cliproxyapi-quota-handoff"}, "capabilities": map[string]bool{
		"scheduler": true, "scheduler_across_priorities": true, "request_interceptor": true, "request_lifecycle_plugin": true, "response_interceptor": true, "response_stream_interceptor": true, "usage_plugin": true, "management_api": true,
	}}
}

func value(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func decodeClaim(headers http.Header) (*handoff.Claim, error) {
	h := http.Header{}
	for k, v := range headers {
		h[http.CanonicalHeaderKey(k)] = v
	}
	raw := h.Get(ControlHeader)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 4096 {
		return nil, fmt.Errorf("quota header is too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var q handoff.Claim
	if err = json.Unmarshal(data, &q); err != nil || !q.Valid() {
		return nil, fmt.Errorf("invalid quota claim")
	}
	return &q, nil
}
func termination(code, message string) pluginapi.RequestInterceptResponse {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message, "type": "quota_handoff"}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: 409, ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "X-Quota-Protocol": {"1"}}, ResponseBody: body}
}
func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		var cfg struct {
			handoff.Config       `yaml:",inline"`
			NotificationsEnabled *bool `yaml:"notifications_enabled"`
		}
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return nil, err
		}
		if err := validateBackend(cfg.Adaptive); err != nil {
			return nil, err
		}
		usageURL, err := validateUsageAuthURL(cfg.UsageAuthURL)
		if err != nil {
			return nil, err
		}
		// Delivery can be changed live without reopening quota state or altering
		// routing, accounting, outstanding reservations or calibration.
		normalized, _ := json.Marshal(cfg.Config)
		digest := handoff.Digest(string(normalized))
		if p.Engine != nil {
			if p.configDigest == digest {
				p.configureNotifications(cfg.NotificationsEnabled)
				return registration(), nil
			}
			return nil, fmt.Errorf("quota configuration changes require an idle plugin reload")
		}
		p.configDigest = digest
		e, err := handoff.Open(cfg.Config, time.Now)
		if err != nil {
			return nil, err
		}
		p.Engine = e
		p.usageAuthURL = usageURL
		p.configureNotifications(cfg.NotificationsEnabled)
		go p.exchange()
		return registration(), nil
	}
	if method == pluginabi.MethodPluginQuiesce || method == pluginabi.MethodPluginShutdown {
		p.stopped.Store(true)
		return struct{}{}, nil
	}
	if p.Engine == nil {
		return nil, fmt.Errorf("quota plugin is not configured")
	}
	switch method {
	case pluginabi.MethodManagementRegister:
		return map[string]any{"resources": []map[string]string{{"Path": "/events", "Description": "Session capability authenticated quota events"}}}, nil
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Method != "GET" || !strings.HasSuffix(req.Path, "/events") {
			return resource(404, map[string]string{"error": "not_found"}), nil
		}
		if req.Query.Get("view") == "usage" {
			return p.usage(req), nil
		}
		authorization := req.Headers.Get("Authorization")
		if authorization == "" {
			return resource(200, map[string]any{"protocol": 1, "mode": p.Engine.AdaptiveConfig().Mode}), nil
		}
		token := strings.TrimPrefix(authorization, "Bearer ")
		if token == authorization || len(token) < 40 || len(token) > 128 {
			return resource(401, map[string]string{"error": "unauthorized"}), nil
		}
		cursor, _ := strconv.ParseInt(req.Query.Get("after"), 10, 64)
		// Authenticate before any upstream probe so an invalid capability cannot spend API quota.
		snapshot, err := p.Engine.Snapshot(token, cursor, req.Query.Get("model"))
		if err != nil {
			return resource(401, map[string]string{"error": "unauthorized"}), nil
		}
		go p.refresh(nil)
		go p.exchange()
		snapshot, err = p.Engine.Snapshot(token, cursor, req.Query.Get("model"))
		if err != nil {
			return resource(503, map[string]string{"error": "quota_state_unavailable"}), nil
		}
		// Keep the durable engine events and cursor even while delivery is muted.
		// Clients advance the cursor without receiving model-visible notices.
		p.logQuotaEvents(snapshot.Events)
		if p.notificationsMuted.Load() {
			snapshot.Events = []handoff.Event{}
		}
		return resource(200, snapshot), nil
	case pluginabi.MethodRequestInterceptBefore:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		q, err := decodeClaim(req.Headers)
		if err != nil {
			return termination("quota_protocol_invalid", "Invalid quota coordination header"), nil
		}
		if q != nil && value(req.Metadata, "caller_scope") == "" {
			return termination("quota_identity_missing", "Quota routing requires an authenticated caller"), nil
		}
		p.mu.Lock()
		p.requests[req.RequestID] = tracked{Claim: q, Caller: value(req.Metadata, "caller_scope"), Legacy: newLegacy(q), Estimate: adaptive.Describe(req.RequestID, req.Model, req.Body), Started: time.Now().Unix()}
		t := p.requests[req.RequestID]
		t.Estimate.TraceID = req.TraceID
		p.requests[req.RequestID] = t
		p.mu.Unlock()
		p.refresh(raw)
		go p.exchange()
		return pluginapi.RequestInterceptResponse{Headers: http.Header{reservationHeader: {req.RequestID}}}, nil
	case pluginabi.MethodSchedulerPick:
		if p.Engine.AdaptiveConfig().Shadow() {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		// CPA's ordinary HTTP execution uses the mixed scheduler even when its
		// model resolves to just one provider. That ABI form leaves Provider empty.
		if req.Provider == "" {
			providers := map[string]bool{}
			for _, candidate := range req.Candidates {
				providers[candidate.Provider] = true
			}
			if len(providers) == 1 {
				for provider := range providers {
					req.Provider = provider
				}
			} else if len(req.Providers) == 1 {
				req.Provider = req.Providers[0]
			}
		}
		protected := false
		for _, credential := range p.Engine.Credentials() {
			if credential.Provider == req.Provider {
				protected = true
				break
			}
			if req.Provider == "" {
				for _, candidate := range req.Candidates {
					if candidate.Provider == credential.Provider {
						protected = true
					}
				}
			}
		}
		q, err := decodeClaim(req.Options.Headers)
		if err != nil {
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "quota_protocol_invalid", RejectReason: "Invalid quota claim"}, nil
		}
		if !protected {
			if q == nil {
				return pluginapi.SchedulerPickResponse{Handled: false}, nil
			}
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "quota_provider_unsupported", RejectReason: "Quota coordination is not available for this provider"}, nil
		}
		if req.Provider == "" {
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "quota_provider_ambiguous", RejectReason: "Quota coordination requires a route resolving to one provider"}, nil
		}
		ids := []string{}
		for _, c := range req.Candidates {
			ids = append(ids, c.ID)
		}
		estimate, trackedID := p.adaptiveRequest(req)
		f, err := p.Engine.Pick(value(req.Options.Metadata, "caller_scope"), req.Provider, req.Model, ids, q, estimate)
		if err != nil {
			code := "quota_state_unavailable"
			if r, ok := err.(*handoff.Refusal); ok {
				code = r.Code
			}
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: code, RejectReason: err.Error()}, nil
		}
		p.selected(trackedID, f)
		if q != nil {
			p.mu.Lock()
			for id, t := range p.requests {
				if t.Claim != nil && t.Claim.Request == q.Request && t.Caller == value(req.Options.Metadata, "caller_scope") && t.Claim.Installation == q.Installation && t.Claim.Session == q.Session && t.Claim.Route == q.Route {
					t.Selected = f
					p.requests[id] = t
				}
			}
			p.mu.Unlock()
		}
		return pluginapi.SchedulerPickResponse{AuthID: f.Auth, Handled: true}, nil
	case pluginabi.MethodRequestInterceptAfter:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		p.mu.Lock()
		t := p.requests[req.RequestID]
		if t.Legacy != nil {
			t.Legacy.auth = value(req.Metadata, "selected_auth_id")
		}
		p.mu.Unlock()
		if p.Engine.AdaptiveConfig().Shadow() && t.Estimate.ID != "" {
			auth := value(req.Metadata, "selected_auth_id")
			if c, ok := p.Engine.Credential(auth); ok {
				f, err := p.Engine.Pick(t.Caller, c.Provider, req.Model, []string{auth}, t.Claim, t.Estimate)
				if err == nil {
					p.selected(req.RequestID, f)
					t.Selected = f
				}
			}
		}
		if !p.Engine.AdaptiveConfig().Shadow() && (t.Claim != nil || p.Engine.AdaptiveConfig().Enforce()) && (t.Selected.Auth == "" || t.Selected.Auth != value(req.Metadata, "selected_auth_id")) {
			return termination("quota_binding_mismatch", "Quota binding differs from the host's selected credential"), nil
		}
		if err := p.Engine.StartAdmission(t.Selected.Admission, value(req.Metadata, "selected_auth_id")); err != nil && !p.Engine.AdaptiveConfig().Shadow() {
			code := "quota_reservation_missing"
			if refusal, ok := err.(*handoff.Refusal); ok {
				code = refusal.Code
			}
			return termination(code, err.Error()), nil
		}
		// Reporting cannot turn an admitted inference request into a refusal.
		_ = p.Engine.ConfirmUsage(t.Selected.Admission, value(req.Metadata, "selected_auth_id"))
		p.logQuotaEvents(p.Engine.LegacyWarnings(value(req.Metadata, "selected_auth_id"), req.Model, t.Estimate))
		clear := []string{ControlHeader, AckHeader, "X-Quota-Protocol", reservationHeader}
		for k := range req.Headers {
			if strings.HasPrefix(strings.ToLower(k), "x-quota-") {
				clear = append(clear, k)
			}
		}
		return pluginapi.RequestInterceptResponse{ClearHeaders: clear}, nil
	case pluginabi.MethodResponseInterceptAfter:
		var req pluginapi.ResponseInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		p.observe(req.RequestID, req.Metadata, req.ResponseHeaders)
		return pluginapi.ResponseInterceptResponse{Headers: p.ack(req.RequestID)}, nil
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req pluginapi.StreamChunkInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.ChunkIndex != -1 {
			return p.legacyChunk(req), nil
		}
		p.observe(req.RequestID, req.Metadata, req.ResponseHeaders)
		p.prepareLegacy(req)
		return pluginapi.StreamChunkInterceptResponse{Headers: p.ack(req.RequestID)}, nil
	case pluginabi.MethodUsageHandle:
		var req pluginapi.UsageRecord
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if err := p.accountUsage(req); err != nil {
			return nil, err
		}
		if err := p.Engine.Observe(req.AuthID, p.coverage(req.RequestID, quota.Headers(req.Provider, req.ResponseHeaders, time.Now().Unix()))); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	case pluginabi.MethodRequestComplete:
		var req pluginapi.RequestCompletion
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		p.mu.Lock()
		t := p.requests[req.RequestID]
		delete(p.requests, req.RequestID)
		p.mu.Unlock()
		if t.Selected.Admission != "" {
			p.Engine.Complete(t.Selected.Admission, req.Outcome == pluginapi.RequestCompletionSucceeded)
		}
		return struct{}{}, nil
	}
	return nil, fmt.Errorf("unsupported quota plugin method %s", method)
}
func (p *Plugin) ack(id string) http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.requests[id]
	if t.Claim == nil {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"protocol": 1, "generation": t.Selected.Generation, "alias": t.Selected.Alias})
	return http.Header{"X-Quota-Protocol": {"1"}, AckHeader: {base64.RawURLEncoding.EncodeToString(body)}}
}
func (p *Plugin) observe(requestID string, metadata map[string]any, headers http.Header) {
	id := value(metadata, "selected_auth_id")
	if id == "" {
		p.mu.Lock()
		t := p.requests[requestID]
		id = t.Selected.Auth
		if t.Legacy != nil {
			id = t.Legacy.auth
		}
		p.mu.Unlock()
	}
	c, ok := p.Engine.Credential(id)
	if !ok {
		return
	}
	_ = p.Engine.Observe(id, p.coverage(requestID, quota.Headers(c.Provider, headers, time.Now().Unix())))
}
func resource(code int, body any) pluginapi.ManagementResponse {
	data, _ := json.Marshal(body)
	return pluginapi.ManagementResponse{StatusCode: code, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: data}
}
