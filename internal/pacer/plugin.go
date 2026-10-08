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
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/management"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/policyadapter"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/policyevidence"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/usagecoverage"
	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type Host func(string, any) (json.RawMessage, error)
type Config struct {
	ForwardReportPath string       `yaml:"forward_report_path" json:"-"`
	ForwardReportBook string       `yaml:"forward_report_book" json:"-"`
	ReportPath        string       `yaml:"report_path"`
	CachePolicy       PolicyConfig `yaml:"cache_policy"`
	Enabled           bool         `yaml:"enabled"`
	Priority          int          `yaml:"priority"`
	StatePath         string       `yaml:"state_path"`
	HistoryPath       string       `yaml:"history_path"`
	CompactOnSwitch   bool         `yaml:"compact_on_switch"`
	UsageAuthURL      string       `yaml:"usage_auth_url"`
	Routing           struct {
		Mode      string   `yaml:"mode"`
		Providers []string `yaml:"providers"`
	} `yaml:"routing"`
}
type credential struct {
	ID, Index, Provider, Account, Alias string
	AccountVerified                     bool // Host billing identity, not a credential-only fallback.
	ExactIdentity                       bool
	CoverageMembership                  string
}
type attempt struct {
	Model, PreviousAccount     string
	Claim                      *tracker.Claim
	Caller, Session, Key, Auth string
	Headers                    http.Header
	Decision                   routing.Decision
	RoutingChoice              pluginapi.SchedulerPickResponse
	RoutingProvider            string
	Binding                    tracker.Binding
}
type Plugin struct {
	forward            *forwardPublisher
	forwardOptions     *forwardOptions
	coverage           *usagecoverage.Consumer
	coverageGroups     map[string]coverageGroup
	coverageMu         sync.Mutex
	coverageCursor     string
	coverageGeneration uint64
	coverageStats      coverageDiagnostics
	policyEvidence     *policyevidence.Store
	reports            *historyreports.Reader
	reportSlots        chan struct{}
	routingEligible    bool // Private rollout gate: false in every runtime constructor/configuration.
	policy             *policyadapter.Adapter
	selections         map[string]pluginapi.CachePolicy
	history            *history.Store
	observationMu      sync.Mutex
	measurement        *measurement.Store
	measurements       map[string]measurement.Report
	measurementErrors  int64
	measurementSource  string
	mu                 sync.Mutex
	Host               Host
	now                func() time.Time
	db                 *sql.DB
	chats              *tracker.Store
	observer           *observation.Store
	cfg                Config
	creds              map[string]credential
	requests           map[string]attempt
	decisionErrors     int64
	workerOnce         sync.Once
	workerDone         chan struct{}
	workerWait         sync.WaitGroup
	refreshMu          sync.Mutex
	discoverMu         sync.Mutex
	lastDiscover       time.Time
	stopped            bool
}

func New(host Host) *Plugin {
	return &Plugin{Host: host, now: time.Now, creds: map[string]credential{}, requests: map[string]attempt{}, selections: map[string]pluginapi.CachePolicy{}, workerDone: make(chan struct{}), reportSlots: make(chan struct{}, 2)}
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
	if p.stopped {
		return nil, errors.New("Pacer is quiesced; create a new instance for reload")
	}
	if p.db != nil {
		if p.cfg.ForwardReportPath != cfg.ForwardReportPath || p.cfg.ForwardReportBook != cfg.ForwardReportBook {
			return nil, errors.New("forward report configuration changes require idle reload")
		}
		if p.cfg.ReportPath != cfg.ReportPath {
			return nil, errors.New("report_path change requires idle reload")
		}
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
	forward, err := p.openForward(cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if forward != nil && p.forward != forward {
			forward.close()
		}
	}()
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
	// Optional marker telemetry does not authorize policy operations and must
	// never turn an evidence-store failure into a policy reset or fake readback.
	// Its compiled namespace must precede the guarded measurement feed boundary.
	p.policyEvidence, err = policyevidence.Open(db, p.now)
	if err != nil {
		p.measurementErrors++
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
	if cfg.ReportPath != "" {
		r, err := historyreports.Open(context.Background(), cfg.ReportPath)
		if err != nil {
			db.Close()
			if p.history != nil {
				p.history.Close()
				p.history = nil
			}
			return nil, errors.New("configured frozen report artifact is unavailable; prepare it offline first")
		}
		p.reports = r
	}
	p.db, p.chats, p.cfg = db, chats, cfg
	p.observer = observer
	p.policy = policy
	p.forward = forward
	return registration(p.policy != nil), nil
}

func (p *Plugin) Close() {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.workerDone)
	}
	p.mu.Unlock()
	p.stopForward(true)
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
	if p.reports != nil {
		_ = p.reports.Close()
		p.reports = nil
	}
}

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
		result, err := p.configure(raw)
		if err == nil {
			p.discover()
			p.startWorker()
			p.startForward()
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
		p.stopForward(false)
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
		result, err := p.policy.Dispatch(context.Background(), method, raw)
		if method == pluginabi.MethodCachePolicyHandle && err == nil && p.policyEvidence != nil {
			var request pluginapi.CachePolicyRequest
			if json.Unmarshal(raw, &request) == nil && request.Method == http.MethodGet {
				if response, ok := result.(pluginapi.CachePolicyResponse); ok && response.StatusCode == http.StatusOK {
					body, evidenceErr := p.policyEvidence.AugmentAuthorized(context.Background(), request.Scope.CallerScope, response.Body)
					if evidenceErr == nil {
						response.Body = body
						result = response
					} else {
						p.metricError(evidenceErr)
					}
				}
			}
		}
		if method == pluginabi.MethodCachePolicyResolve && err == nil {
			var request struct {
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(raw, &request)
			if selection, ok := result.(pluginapi.CachePolicy); ok {
				p.mu.Lock()
				p.selections[request.RequestID] = selection
				p.mu.Unlock()
			}
		}
		return result, err
	case pluginabi.MethodManagementRegister:
		return map[string]any{"resources": []map[string]string{{"Path": "/index.html", "Menu": "Pacer", "Description": "Quota-aware routing and usage"}, {"Path": "/events", "Description": "Compatible capability-authenticated usage"}}, "routes": append([]map[string]string{{"Method": "GET", "Path": "/plugins/pacer/state"}, {"Method": "PATCH", "Path": "/plugins/pacer/routing"}, {"Method": "GET", "Path": "/plugins/pacer/observations"}, {"Method": "POST", "Path": "/plugins/pacer/observations/reconcile"}, {"Method": "GET", "Path": "/plugins/pacer/keys"}, {"Method": "PATCH", "Path": "/plugins/pacer/keys"}}, reportRoutes()...)}, nil
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
		return pluginapi.ResponseInterceptResponse{Headers: p.ack(req.RequestID), ClearHeaders: quotaAckHeaders()}, nil
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req pluginapi.StreamChunkInterceptRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.ChunkIndex == -1 {
			p.observe(req.RequestID, req.Metadata, req.ResponseHeaders, p.now())
			return pluginapi.StreamChunkInterceptResponse{Headers: p.ack(req.RequestID), ClearHeaders: quotaAckHeaders()}, nil
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
		delete(p.selections, req.RequestID)
		p.mu.Unlock()
		return struct{}{}, nil
	case pluginabi.MethodUsageHandle:
		defer p.WakeForward()
		captured, err := p.recordUsageCaptured(raw)
		if err != nil {
			return nil, pluginabi.NewError("pacer_usage_unavailable", "Native usage could not be durably recorded", 503)
		}
		var req struct {
			pluginapi.UsageRecord
			ResponseHeadersObservedAt map[string]time.Time
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			p.coverageFailure("usage_envelope_invalid", captured.Account)
			return nil, pluginabi.NewError("pacer_usage_unavailable", "Native usage envelope is invalid", 503)
		}
		if len(req.ResponseHeadersObservedAt) > 0 {
			p.observeUsageCaptured(captured, req.ResponseHeaders, req.ResponseHeadersObservedAt, req.RequestedAt)
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
		p.observeUsageCaptured(captured, h, nil, req.RequestedAt)
		return struct{}{}, nil
	}
	return nil, fmt.Errorf("unsupported Pacer method %s", method)
}

func (p *Plugin) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	p.mu.Lock()
	cfg, eligible := p.cfg, p.routingEligible
	id := header(http.Header(req.Options.Headers), requestHeader)
	t, tracked := p.requests[id]
	selection, managed := p.selections[id]
	// Every scheduler retry supersedes the previous proposal, including fallbacks.
	t.RoutingChoice, t.Decision = pluginapi.SchedulerPickResponse{}, routing.Decision{}
	if tracked {
		p.requests[id] = t
	}
	p.mu.Unlock()
	provider := req.Provider
	for _, candidate := range req.Candidates {
		if provider == "" {
			provider = candidate.Provider
		}
		if candidate.Provider != provider {
			return pluginapi.SchedulerPickResponse{}
		}
	}
	enabled := false
	for _, v := range cfg.Routing.Providers {
		if v == provider {
			enabled = true
		}
	}
	if !cfg.Enabled || !enabled || !tracked || (t.Claim != nil && t.Claim.Kind != "primary") || (managed && selection.Kind != "primary") {
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
	expected := 0.0
	candidates := []credential{}
	p.mu.Lock()
	for _, candidate := range req.Candidates {
		c, ok := p.creds[candidate.ID]
		if !ok || c.Provider != provider {
			continue
		}
		candidates = append(candidates, c)
		windows, readErr := p.observer.ReadWindows(c.Account)
		if readErr != nil {
			continue
		}
		account := routing.Account{Auth: c.ID, Account: c.Account, Budgets: routingBudgets(provider, req.Model, windows, p.now())}
		if report, ok := p.measurements[c.Account]; ok && p.now().Sub(report.AsOf) < time.Minute {
			if managed {
				prior := report.Expected(measurement.ChatRef{CallerID: selection.Scope.CallerScope, InstallationID: selection.Scope.InstallationID, SessionID: selection.Scope.SessionID, ProviderRoute: selection.Scope.ProviderRoute, Provider: provider, AccountID: c.Account, ContextEpoch: selection.ContextEpoch}, measurement.TTL(selection.TTL))
				if prior.Known && prior.USD > expected {
					expected = prior.USD
				}
			}
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
	d := routing.Pick(routing.Default(), p.now(), routing.Chat{Account: b.Account, ProtectedUntil: b.ProtectedUntil, UnknownWarmth: b.UnknownWarmth, InFlight: active, ExpectedValue: expected}, accounts)
	p.mu.Lock()
	t.Key = key
	t.Decision = d
	t.Model, t.PreviousAccount = req.Model, b.Account
	t.RoutingProvider = provider
	t.RoutingChoice = routingAction(req, cfg.Routing.Mode, eligible, d, candidates)
	p.requests[header(http.Header(req.Options.Headers), requestHeader)] = t
	p.mu.Unlock()
	return t.RoutingChoice
}

func (p *Plugin) after(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	auth := text(req.Metadata, "selected_auth_id")
	p.mu.Lock()
	t, ok := p.requests[req.RequestID]
	c, known := p.creds[auth]
	gate := p.cfg.CompactOnSwitch
	selectedPolicy, managed := p.selections[req.RequestID]
	managedAux := managed && selectedPolicy.Kind != "primary"
	p.mu.Unlock()
	if !known && auth != "" {
		// CPA may finish loading credentials after plugin registration. Refresh
		// its local inventory on a newly selected ID; never call analytics here.
		p.discover(true)
		p.mu.Lock()
		c, known = p.creds[auth]
		p.mu.Unlock()
	}
	if ok && !known && (managed || t.Claim != nil) {
		return refuse("quota_state_unavailable", "Selected credential identity could not be verified")
	}
	if ok && known {
		if err := p.captureUsageCredential(req.RequestID, c); err != nil {
			return refuse("quota_state_unavailable", "Pacer usage attribution could not be recorded")
		}
		key := chatKey(t.Caller, c.Provider, req.Model, t.Session, t.Claim)
		if key != "" && !managedAux {
			b, err := p.chats.Admit(req.RequestID, key, selectionIntent(t, c, req.Model), t.Claim, gate)
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
	if t.Claim == nil || t.Key == "" || t.Auth == "" {
		return nil
	}
	if t.Binding.Key == "" {
		// First auxiliaries on a separate provider route have no primary binding
		// to acknowledge. Admit has authenticated their claim without creating
		// one. Prove only this helper request; never invent a generation/alias.
		switch t.Claim.Kind {
		case "compaction", "title", "generate":
			raw, _ := json.Marshal(map[string]any{"protocol": 1, "kind": t.Claim.Kind, "request": t.Claim.Request})
			return http.Header{"X-Quota-Protocol": {"1"}, "X-Quota-Auxiliary": {base64.RawURLEncoding.EncodeToString(raw)}}
		}
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"protocol": 1, "generation": t.Binding.Generation, "alias": t.Binding.Alias})
	return http.Header{"X-Quota-Protocol": {"1"}, "X-Quota-Binding": {base64.RawURLEncoding.EncodeToString(raw)}}
}

// Upstream responses cannot supply coordinator acknowledgements. The host
// removes these before applying the exact headers from this admitted attempt.
func quotaAckHeaders() []string {
	return []string{"X-Quota-Protocol", "X-Quota-Binding", "X-Quota-Auxiliary"}
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
	if strings.HasPrefix(req.Path, historyRoot) {
		return p.reportsAPI(req)
	}
	if req.Method == "PATCH" && req.Path == "/v0/management/plugins/pacer/keys" {
		return p.editKeys(req)
	}
	if req.Method == "GET" && req.Path == "/v0/management/plugins/pacer/keys" {
		return p.keysView()
	}
	if (req.Method == "GET" && req.Path == "/v0/management/plugins/pacer/observations") || (req.Method == "POST" && req.Path == "/v0/management/plugins/pacer/observations/reconcile") {
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
		p.discover() // local inventory only, bounded separately from quota reconciliation
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
			for _, w := range windows {
				state := windowState(w, p.now())
				views = append(views, map[string]any{"scope": w.Scope, "seconds": w.Seconds, "remaining": w.Remaining, "reset": w.Reset, "state": state, "source": w.Usage.Source, "usage_evidence": w.Usage, "reset_evidence": w.ResetEvidence})
			}
			job, _, err := p.observer.Status(c.Account)
			if err != nil {
				return resource(503, map[string]string{"error": "observation_unavailable"})
			}
			row := map[string]any{"id": c.ID, "auth_index": c.Index, "alias": c.Alias, "provider": c.Provider, "state": accountState(c.Provider, windows, p.now()), "windows": views, "reconciliation": map[string]any{"next": job.Next, "blocked": job.Blocked}}
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
		historyCoverage := "No prepared native historical report artifact is configured. Keep CPAMP and its database available."
		if p.reports != nil {
			pin := p.reports.DefaultPin()
			historyCoverage = fmt.Sprintf("Frozen reports at %s, event checkpoint %d. Native future usage is separate. Legacy views, real-data migration and retirement gates remain unverified.", time.UnixMilli(pin.At).UTC().Format(time.RFC3339), pin.Through)
		}
		return resource(200, map[string]any{"history_available": p.reports != nil, "history_coverage": historyCoverage, "routing": map[string]any{"mode": p.cfg.Routing.Mode, "can_activate": false, "gate": "Active routing awaits integration acceptance and 24–48 hours of dry-run observation."}, "accounts": accounts, "decisions": decisions, "decision_write_errors": p.decisionErrors, "measurement": map[string]any{"delivery": "native-acknowledged-scoped", "coverage": "Account membership history and physical carry-in remain unproven; capacity stays unmeasured.", "errors": p.measurementErrors, "source_checkpoint": map[string]any{"calls": p.coverageStats.Calls, "errors": p.coverageStats.Errors, "stale": p.coverageStats.Stale, "reason": p.coverageStats.Reason, "domain": "request_start_only", "account_membership_complete": false, "physical_carry_in_proven": false}}})
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
