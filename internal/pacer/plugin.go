// Package pacer adapts the host ABI. Routing, quota observation, policy state and
// the management page have separate owners; inference never queries quota APIs.
package pacer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/management"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/policyadapter"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type Host func(string, any) (json.RawMessage, error)
type Config struct {
	CachePolicy     PolicyConfig `yaml:"cache_policy"`
	Enabled         bool         `yaml:"enabled"`
	Priority        int          `yaml:"priority"`
	StatePath       string       `yaml:"state_path"`
	HistoryPath     string       `yaml:"history_path"`
	CompactOnSwitch bool         `yaml:"compact_on_switch"`
	UsageAuthURL    string       `yaml:"usage_auth_url"`
	Routing         struct {
		Mode      string   `yaml:"mode"`
		Providers []string `yaml:"providers"`
	} `yaml:"routing"`
}
type credential struct{ ID, Index, Provider, Account, Alias string }
type attempt struct {
	Model, PreviousAccount     string
	Claim                      *tracker.Claim
	Caller, Session, Key, Auth string
	Headers                    http.Header
	Decision                   routing.Decision
	Binding                    tracker.Binding
}
type Plugin struct {
	policy            *policyadapter.Adapter
	history           *history.Store
	observationMu     sync.Mutex
	measurement       *measurement.Store
	measurements      map[string]measurement.Report
	measurementErrors int64
	mu                sync.Mutex
	Host              Host
	now               func() time.Time
	db                *sql.DB
	chats             *tracker.Store
	observer          *observation.Store
	cfg               Config
	creds             map[string]credential
	requests          map[string]attempt
	decisionErrors    int64
	workerOnce        sync.Once
	workerDone        chan struct{}
	workerWait        sync.WaitGroup
	refreshMu         sync.Mutex
	discoverMu        sync.Mutex
	lastDiscover      time.Time
	stopped           bool
}

func New(host Host) *Plugin {
	return &Plugin{Host: host, now: time.Now, creds: map[string]credential{}, requests: map[string]attempt{}, workerDone: make(chan struct{})}
}

func registration(policyEnabled ...bool) any {
	schema := 6
	enabled := len(policyEnabled) > 0 && policyEnabled[0]
	if enabled {
		schema = 7
	}
	return map[string]any{"schema_version": schema, "metadata": pluginapi.Metadata{Name: "pacer", Version: "0.2.0-dev", Author: "floris3456", GitHubRepository: "https://github.com/floris3456/cliproxyapi-quota-handoff"}, "capabilities": map[string]bool{"scheduler": true, "scheduler_across_priorities": true, "request_interceptor": true, "request_lifecycle_plugin": true, "response_interceptor": true, "response_stream_interceptor": true, "usage_plugin": true, "management_api": true, "cache_policy": enabled}}
}

func (p *Plugin) configure(raw []byte) (any, error) {
	var request struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	var cfg Config
	if len(request.ConfigYAML) == 0 {
		return nil, errors.New("state_path is required")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(request.ConfigYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.StatePath == "" {
		return nil, errors.New("state_path is required")
	}
	policyOpts, err := p.policyOptions(cfg.CachePolicy)
	if err != nil {
		return nil, err
	}
	if cfg.CachePolicy.Enabled && request.SchemaVersion < 7 {
		return nil, errors.New("managed cache policy requires CPA schema 7")
	}
	usageURL, err := validateUsageURL(cfg.UsageAuthURL)
	if err != nil {
		return nil, err
	}
	cfg.UsageAuthURL = usageURL
	if cfg.Routing.Mode == "" {
		cfg.Routing.Mode = "off"
	}
	if cfg.Routing.Mode != "off" && cfg.Routing.Mode != "dry-run" {
		return nil, errors.New("active routing requires completed rollout verification; candidate supports off or dry-run")
	}
	if len(cfg.Routing.Providers) == 0 {
		cfg.Routing.Providers = []string{"claude"}
	}
	for _, provider := range cfg.Routing.Providers {
		if provider != "claude" && provider != "codex" {
			return nil, errors.New("unsupported routing provider")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db != nil {
		if !reflect.DeepEqual(p.cfg.CachePolicy, cfg.CachePolicy) {
			return nil, errors.New("cache_policy changes require an idle reload")
		}
		if p.cfg.HistoryPath != cfg.HistoryPath {
			return nil, errors.New("history_path change requires idle reload")
		}
		if p.cfg.StatePath != cfg.StatePath {
			return nil, errors.New("state_path change requires idle reload")
		}
		mode, err := reconcileMode(p.db, cfg.Routing.Mode)
		if err != nil {
			return nil, err
		}
		cfg.Routing.Mode = mode
		p.cfg = cfg
		return registration(p.policy != nil), nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0700); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_RDWR
	if cfg.CachePolicy.Enabled {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(cfg.StatePath, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", cfg.StatePath+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var policy *policyadapter.Adapter
	if cfg.CachePolicy.Enabled {
		policy, err = policyadapter.Open(context.Background(), db, policyOpts)
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := openDecisions(db); err != nil {
		db.Close()
		return nil, err
	}
	mode, err := reconcileMode(db, cfg.Routing.Mode)
	if err != nil {
		db.Close()
		return nil, err
	}
	cfg.Routing.Mode = mode
	chats, err := tracker.Open(db, p.now)
	if err != nil {
		db.Close()
		return nil, err
	}
	observer, err := observation.Open(db, p.now)
	if err != nil {
		db.Close()
		return nil, err
	}
	measure, err := p.openMeasurements(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	p.measurement = measure
	p.measurements = map[string]measurement.Report{}
	if cfg.HistoryPath != "" {
		if _, err = os.Stat(cfg.HistoryPath); err != nil {
			db.Close()
			return nil, errors.New("configured history archive is unavailable; import it offline first")
		}
		h, err := history.Open(cfg.HistoryPath)
		if err != nil {
			db.Close()
			return nil, errors.New("configured history archive is incompatible")
		}
		p.history = h
	}
	p.db, p.chats, p.cfg = db, chats, cfg
	p.observer = observer
	p.policy = policy
	return registration(p.policy != nil), nil
}

func (p *Plugin) Close() {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.workerDone)
	}
	p.mu.Unlock()
	p.workerWait.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db != nil {
		_ = p.db.Close()
		p.db = nil
	}
	if p.history != nil {
		_ = p.history.Close()
		p.history = nil
	}
}

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
		result, err := p.configure(raw)
		if err == nil {
			p.discover()
			p.startWorker()
		}
		return result, err
	}
	if method == pluginabi.MethodPluginShutdown || method == pluginabi.MethodPluginQuiesce {
		p.mu.Lock()
		if !p.stopped {
			p.stopped = true
			close(p.workerDone)
		}
		p.mu.Unlock()
		return struct{}{}, nil
	}
	p.mu.Lock()
	ready := p.db != nil && !p.stopped
	p.mu.Unlock()
	if !ready {
		return nil, errors.New("Pacer state unavailable")
	}
	switch method {
	case pluginabi.MethodCachePolicyHandle, pluginabi.MethodCachePolicyResolve, pluginabi.MethodCachePolicyAttempt:
		if p.policy == nil {
			return nil, pluginabi.NewError("pacer_cache_unavailable", "Managed cache policy is not configured", 503)
		}
		return p.policy.Dispatch(context.Background(), method, raw)
	case pluginabi.MethodManagementRegister:
		return map[string]any{"resources": []map[string]string{{"Path": "/index.html", "Menu": "Pacer", "Description": "Quota-aware routing and usage"}, {"Path": "/events", "Description": "Compatible capability-authenticated usage"}}, "routes": []map[string]string{{"Method": "GET", "Path": "/plugins/pacer/state"}, {"Method": "PATCH", "Path": "/plugins/pacer/routing"}, {"Method": "GET", "Path": "/plugins/pacer/quota"}, {"Method": "POST", "Path": "/plugins/pacer/quota/reconcile"}, {"Method": "GET", "Path": "/plugins/pacer/keys"}}}, nil
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return p.manage(req), nil
	case pluginabi.MethodRequestInterceptBefore:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		q, err := claim(req.Headers)
		if err != nil {
			return refuse("quota_protocol_invalid", "Invalid quota coordination header"), nil
		}
		caller := text(req.Metadata, "caller_scope")
		if q != nil && caller == "" {
			return refuse("quota_identity_missing", "Quota routing requires authenticated caller identity"), nil
		}
		p.mu.Lock()
		p.requests[req.RequestID] = attempt{Claim: q, Caller: caller, Session: session(req.Headers, req.Metadata), Headers: req.Headers}
		p.mu.Unlock()
		return pluginapi.RequestInterceptResponse{Headers: http.Header{requestHeader: {req.RequestID}}}, nil
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return p.pick(req), nil
	case pluginabi.MethodRequestInterceptAfter:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return p.after(req), nil
	case pluginabi.MethodResponseInterceptAfter:
		var req pluginapi.ResponseInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		p.observe(req.RequestID, req.Metadata, req.ResponseHeaders, p.now())
		return pluginapi.ResponseInterceptResponse{Headers: p.ack(req.RequestID)}, nil
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req pluginapi.StreamChunkInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.ChunkIndex == -1 {
			p.observe(req.RequestID, req.Metadata, req.ResponseHeaders, p.now())
			return pluginapi.StreamChunkInterceptResponse{Headers: p.ack(req.RequestID)}, nil
		}
		return pluginapi.StreamChunkInterceptResponse{}, nil
	case pluginabi.MethodRequestComplete:
		if p.policy != nil {
			if _, err := p.policy.Dispatch(context.Background(), method, raw); err != nil {
				return nil, err
			}
		}
		var req pluginapi.RequestCompletion
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		p.chats.Complete(req.RequestID)
		p.mu.Lock()
		delete(p.requests, req.RequestID)
		p.mu.Unlock()
		return struct{}{}, nil
	case pluginabi.MethodUsageHandle:
		var req struct {
			pluginapi.UsageRecord
			ResponseHeadersObservedAt map[string]time.Time
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if len(req.ResponseHeadersObservedAt) > 0 {
			p.observeCaptured(req.AuthID, req.ResponseHeaders, req.ResponseHeadersObservedAt)
			return struct{}{}, nil
		}
		// Older hosts do not supply the response-capture clock. Absolute headers
		// still carry useful evidence; strip countdowns rather than re-anchor a
		// delayed stream's relative reset to callback time.
		h := req.ResponseHeaders.Clone()
		for key := range h {
			if strings.HasSuffix(strings.ToLower(key), "-reset-after-seconds") {
				delete(h, key)
			}
		}
		p.observeAuth(req.AuthID, h, req.RequestedAt)
		return struct{}{}, nil
	}
	return nil, fmt.Errorf("unsupported Pacer method %s", method)
}

func (p *Plugin) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	provider := req.Provider
	for _, candidate := range req.Candidates {
		if provider == "" {
			provider = candidate.Provider
		}
		if candidate.Provider != provider {
			return pluginapi.SchedulerPickResponse{}
		}
	}
	p.mu.Lock()
	cfg := p.cfg
	t, tracked := p.requests[header(http.Header(req.Options.Headers), requestHeader)]
	p.mu.Unlock()
	enabled := false
	for _, v := range cfg.Routing.Providers {
		if v == provider {
			enabled = true
		}
	}
	if !enabled || !tracked || (t.Claim != nil && t.Claim.Kind != "primary") {
		return pluginapi.SchedulerPickResponse{}
	}
	key := chatKey(t.Caller, provider, req.Model, t.Session, t.Claim)
	if key == "" {
		return pluginapi.SchedulerPickResponse{}
	}
	b, active, err := p.chats.Get(key)
	if err != nil {
		return pluginapi.SchedulerPickResponse{}
	}
	if b.Key == "" && p.Host != nil && t.Session != "" {
		raw, lookupErr := p.Host("host.affinity.lookup", pluginapi.HostAffinityLookupRequest{Provider: provider, Model: req.Model, SessionID: t.Session})
		var affinity pluginapi.HostAffinityLookupResponse
		if lookupErr != nil || json.Unmarshal(raw, &affinity) != nil || affinity.Status == "ambiguous" || affinity.Status == "unsupported" {
			return pluginapi.SchedulerPickResponse{}
		}
		if affinity.Status == "bound" {
			p.mu.Lock()
			for _, c := range p.creds {
				if c.Index == affinity.AuthIndex {
					err = p.chats.Seed(key, tracker.Selection{Auth: c.ID, Account: c.Account, Alias: c.Alias, Provider: provider, Model: req.Model})
					break
				}
			}
			p.mu.Unlock()
			if err != nil {
				return pluginapi.SchedulerPickResponse{}
			}
			b, active, err = p.chats.Get(key)
			if err != nil || b.Key == "" {
				return pluginapi.SchedulerPickResponse{}
			}
		}
	}
	accounts := []routing.Account{}
	p.mu.Lock()
	for _, candidate := range req.Candidates {
		c, ok := p.creds[candidate.ID]
		if !ok || c.Provider != provider {
			continue
		}
		windows, readErr := p.observer.ReadWindows(c.Account)
		if readErr != nil {
			continue
		}
		account := routing.Account{Auth: c.ID, Account: c.Account, Budgets: routingBudgets(provider, req.Model, windows, p.now())}
		if report, ok := p.measurements[c.Account]; ok && p.now().Sub(report.AsOf) < time.Minute {
			if report.SevenDay.Known {
				account.Size = report.SevenDay.USDPerPercent
			}
			if report.FiveHour.Known {
				account.ShortValue = report.FiveHour.USDPerPercent
				account.RunwayUnknown = !report.WarmBurn.Bounded
			}
			if report.WarmBurn.Bounded {
				account.WarmBurn = report.WarmBurn.HighUSDPerHour
			}
		}
		accounts = append(accounts, account)
	}
	p.mu.Unlock()
	d := routing.Pick(routing.Default(), p.now(), routing.Chat{Account: b.Account, ProtectedUntil: b.ProtectedUntil, UnknownWarmth: b.UnknownWarmth, InFlight: active}, accounts)
	p.mu.Lock()
	t.Key = key
	t.Decision = d
	t.Model, t.PreviousAccount = req.Model, b.Account
	p.requests[header(http.Header(req.Options.Headers), requestHeader)] = t
	p.mu.Unlock()
	// Until the dry-run observation and integration gates are accepted the
	// candidate records choices but cannot alter the host's scheduler.
	return pluginapi.SchedulerPickResponse{}
}

func (p *Plugin) after(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	auth := text(req.Metadata, "selected_auth_id")
	p.mu.Lock()
	t, ok := p.requests[req.RequestID]
	c, known := p.creds[auth]
	gate := p.cfg.CompactOnSwitch
	p.mu.Unlock()
	if !known && auth != "" {
		p.discover()
		p.mu.Lock()
		c, known = p.creds[auth]
		p.mu.Unlock()
	}
	if ok && known {
		key := chatKey(t.Caller, c.Provider, req.Model, t.Session, t.Claim)
		if key != "" {
			// In off/dry-run CPA alone made the pick. A changed account therefore
			// retains the established forced-switch checkpoint contract.
			b, err := p.chats.Admit(req.RequestID, key, tracker.Selection{Auth: c.ID, Account: c.Account, Alias: c.Alias, Provider: c.Provider, Model: req.Model, Forced: true}, t.Claim, gate)
			if err != nil {
				if e, yes := err.(*tracker.Refusal); yes {
					p.recordDecision(t, c, e.Code)
					return refuse(e.Code, e.Message)
				}
				return refuse("quota_state_unavailable", "Pacer binding state is unavailable")
			}
			p.mu.Lock()
			t.Key, t.Auth, t.Binding = key, auth, b
			p.requests[req.RequestID] = t
			p.mu.Unlock()
			p.recordDecision(t, c, "admitted")
		}
	}
	clear := []string{controlHeader, requestHeader, "X-Quota-Binding", "X-Quota-Protocol"}
	for key := range req.Headers {
		if strings.HasPrefix(strings.ToLower(key), "x-quota-") {
			clear = append(clear, key)
		}
	}
	return pluginapi.RequestInterceptResponse{ClearHeaders: clear}
}

func (p *Plugin) ack(id string) http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.requests[id]
	if t.Claim == nil || t.Binding.Key == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"protocol": 1, "generation": t.Binding.Generation, "alias": t.Binding.Alias})
	return http.Header{"X-Quota-Protocol": {"1"}, "X-Quota-Binding": {base64.RawURLEncoding.EncodeToString(raw)}}
}

func refuse(code, message string) pluginapi.RequestInterceptResponse {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message, "type": "quota_handoff"}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: 409, ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "X-Quota-Protocol": {"1"}}, ResponseBody: raw}
}

func resource(status int, body any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(body)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: raw}
}

func (p *Plugin) manage(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if req.Method == "GET" && req.Path == "/v0/management/plugins/pacer/keys" {
		return p.keysView()
	}
	if (req.Method == "GET" && req.Path == "/v0/management/plugins/pacer/quota") || (req.Method == "POST" && req.Path == "/v0/management/plugins/pacer/quota/reconcile") {
		return p.quotaAPI(req)
	}
	if req.Method == "GET" && req.Path == "/v0/resource/plugins/pacer/index.html" {
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'"}}, Body: management.Page}
	}
	if req.Method == "GET" && req.Path == "/v0/resource/plugins/pacer/events" {
		return p.events(req)
	}
	// Exact management paths only: the public resource namespace cannot reach
	// these branches. CPA authenticates management routes before ABI dispatch.
	if req.Method == "GET" && req.Path == "/v0/management/plugins/pacer/state" {
		p.mu.Lock()
		defer p.mu.Unlock()
		accounts := []map[string]any{}
		decisions, err := p.decisionView()
		if err != nil {
			return resource(503, map[string]string{"error": "decision_state_unavailable"})
		}
		for _, c := range p.creds {
			windows, err := p.observer.ReadWindows(c.Account)
			if err != nil {
				return resource(503, map[string]string{"error": "observation_unavailable"})
			}
			views := []map[string]any{}
			state := "unknown"
			for _, w := range windows {
				state = w.State(p.now())
				if w.Remaining != nil && w.BalanceAge(p.now()) >= time.Hour {
					state = "stale"
				}
				views = append(views, map[string]any{"scope": w.Scope, "seconds": w.Seconds, "remaining": w.Remaining, "reset": w.Reset, "state": state, "source": w.Usage.Source, "usage_evidence": w.Usage, "reset_evidence": w.ResetEvidence})
			}
			job, _, err := p.observer.Status(c.Account)
			if err != nil {
				return resource(503, map[string]string{"error": "observation_unavailable"})
			}
			row := map[string]any{"id": c.ID, "auth_index": c.Index, "alias": c.Alias, "provider": c.Provider, "state": state, "windows": views, "reconciliation": map[string]any{"next": job.Next, "blocked": job.Blocked}}
			base := routing.Account{Auth: c.ID, Account: c.Account, Budgets: routingBudgets(c.Provider, "", windows, p.now())}
			row["pace_unit"] = "%/h · equal sizes"
			if report, ok := p.measurements[c.Account]; ok {
				row["measurement"] = map[string]any{"as_of": report.AsOf, "five_hour": report.FiveHour, "seven_day": report.SevenDay, "warm_burn": report.WarmBurn, "coverage": report.Coverage}
				if report.SevenDay.Known && p.now().Sub(report.AsOf) < time.Minute {
					base.Size = report.SevenDay.USDPerPercent
					row["pace_unit"] = "$/h"
				}
			}
			if d := routing.Pick(routing.Default(), p.now(), routing.Chat{}, []routing.Account{base}); d.Handled {
				row["pace"] = d.Pace
			}
			accounts = append(accounts, row)
		}
		sort.Slice(accounts, func(i, j int) bool { return fmt.Sprint(accounts[i]["id"]) < fmt.Sprint(accounts[j]["id"]) })
		return resource(200, map[string]any{"routing": map[string]any{"mode": p.cfg.Routing.Mode, "can_activate": false, "gate": "Active routing awaits integration acceptance and 24–48 hours of dry-run observation."}, "accounts": accounts, "decisions": decisions, "decision_write_errors": p.decisionErrors, "measurement": map[string]any{"delivery": "native-in-memory-best-effort", "coverage": "No drained delivery watermark: account-size estimates remain unmeasured.", "errors": p.measurementErrors}})
	}
	if req.Method == "PATCH" && req.Path == "/v0/management/plugins/pacer/routing" {
		if len(req.Body) > 1024 {
			return resource(413, map[string]string{"error": "body_too_large"})
		}
		var body struct {
			Mode string `json:"mode"`
		}
		decoder := json.NewDecoder(bytes.NewReader(req.Body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || (body.Mode != "off" && body.Mode != "dry-run") {
			return resource(409, map[string]string{"error": "routing_rollout_gate", "message": "Only off or dry-run is accepted before rollout verification"})
		}
		if err := p.setMode(body.Mode); err != nil {
			return resource(503, map[string]string{"error": "routing_state_unavailable"})
		}
		return resource(200, map[string]string{"mode": body.Mode})
	}
	return resource(404, map[string]string{"error": "not_found"})
}
