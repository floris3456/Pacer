package pacer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/windowstart"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func startCapability() pluginapi.QuotaWindowStartCapability {
	return pluginapi.QuotaWindowStartCapability{Version: 1, Claude: true, ExactAccount: true, SingleAttempt: true, NoCache: true}
}

func windowPlugin(t *testing.T, path string, clock *atomic.Int64, host Host) *Plugin {
	t.Helper()
	p := New(host)
	p.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	p.workerOnce.Do(func() {}) // Deterministic explicit ticks, no sleeping tests.
	t.Cleanup(p.Close)
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"schema_version": 7, "host_capabilities": map[string]any{"quota_window_start": startCapability()}, "config_yaml": []byte("enabled: true\nstate_path: " + path + "\nquota_window_start:\n  enabled: true\n")})
	account := "claude:" + tracker.Digest("claude:account:verified-fixture")
	p.mu.Lock()
	p.creds = map[string]credential{}
	for _, id := range []string{"one", "two", "three"} {
		p.creds[id] = credential{ID: id, Index: id, Account: account, Provider: "claude", AccountVerified: true, ExactIdentity: true}
	}
	p.mu.Unlock()
	return p
}

func dormantSnapshot(t *testing.T, p *Plugin) {
	t.Helper()
	c := p.creds["one"]
	now := p.now()
	body := []byte(fmt.Sprintf(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":20,"resets_at":%q},"extra_usage":{"is_enabled":false}}`, now.Add(7*24*time.Hour).Format(time.RFC3339)))
	windows, err := observation.API("claude", body, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.credits.ObserveCredits(c.Account, observation.ClaudeExtraUsage(body, now)); err != nil {
		t.Fatal(err)
	}
	p.storeObservations(c, windows)
}

func windowHost(callback func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error)) Host {
	return func(method string, input any) (json.RawMessage, error) {
		switch method {
		case "host.http.operation_open":
			return json.RawMessage(`{"operation_id":"fixture-operation"}`), nil
		case "host.claude.quota_window_start":
			return callback(input.(pluginapi.HostQuotaWindowStartRequest))
		default:
			return testHost(method, input)
		}
	}
}

func TestWindowStartConfigurationRequiresAttestedHostAndIdleReload(t *testing.T) {
	for _, tc := range []struct {
		name, cfg string
		host      pluginapi.QuotaWindowStartCapability
		want      bool
	}{
		{"default disabled", "", pluginapi.QuotaWindowStartCapability{}, true},
		{"unsupported", "quota_window_start:\n  enabled: true\n", pluginapi.QuotaWindowStartCapability{}, false},
		{"configured", "quota_window_start:\n  enabled: true\n", startCapability(), true},
		{"thinking suffix", "quota_window_start:\n  model: claude-haiku-5-5(high)\n", startCapability(), false},
		{"expensive model", "quota_window_start:\n  model: claude-opus-5-5\n", startCapability(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(testHost)
			t.Cleanup(p.Close)
			path := filepath.Join(t.TempDir(), "state.sqlite")
			q := map[string]any{"schema_version": 7, "host_capabilities": map[string]any{"quota_window_start": tc.host}, "config_yaml": []byte("enabled: true\nstate_path: " + path + "\n" + tc.cfg)}
			raw, _ := json.Marshal(q)
			_, err := p.configure(raw)
			if (err == nil) != tc.want {
				t.Fatal(err)
			}
			if !tc.want {
				return
			}
			if tc.name == "default disabled" {
				if _, err := os.Stat(path + ".window-start.sqlite"); !os.IsNotExist(err) {
					t.Fatal("disabled feature created ledger", err)
				}
				return
			}
			if info, err := os.Stat(path + ".window-start.sqlite"); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal(info, err)
			}
			if api := p.clientAPILocked(); !api.Features["claude_quota_window_start"].Enabled {
				t.Fatal("feature not discoverable", api)
			}
			q["config_yaml"] = []byte("enabled: true\nstate_path: " + path + "\n")
			raw, _ = json.Marshal(q)
			if _, err := p.configure(raw); err == nil || !strings.Contains(err.Error(), "idle reload") {
				t.Fatal("live change accepted", err)
			}
		})
	}
}

func TestWindowStartOnceAcrossAliasesCallersAndRestart(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC).UnixNano())
	var calls atomic.Int64
	host := windowHost(func(q pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
		calls.Add(1)
		if q.Version != 1 || q.AuthID != "one" || q.Model != "claude-haiku-4-5-20251001" || q.OperationID != "fixture-operation" || q.AccountKey != "claude:"+tracker.Digest("claude:account:verified-fixture") {
			t.Errorf("incorrect request %+v", q)
		}
		clock.Add(int64(time.Second))
		return json.Marshal(pluginapi.HostQuotaWindowStartResponse{Version: 1, Dispatched: true, StatusCode: 200})
	})
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := windowPlugin(t, path, &clock, host)
	dormantSnapshot(t, p)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Go(p.startDormantWindows)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate request", calls.Load())
	}
	view := p.windowStartView(p.creds["one"])
	if view["state"] != "awaiting_confirmation" {
		t.Fatal(view)
	}
	p.Close()
	clock.Add(int64(time.Hour))
	p = windowPlugin(t, path, &clock, host)
	dormantSnapshot(t, p)
	p.startDormantWindows()
	if calls.Load() != 1 {
		t.Fatal("restart repeated unconfirmed request")
	}
	clock.Add(int64(time.Second))
	reset := p.now().Add(5 * time.Hour)
	p.observeAuth("two", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(reset.Unix(), 10)}}, p.now())
	if view = p.windowStartView(p.creds["one"]); view["state"] != "confirmed" {
		t.Fatal("provider reset not confirmed", view)
	}
	if _, blocked := view["reason"]; blocked {
		t.Fatal("confirmed active window still shows a blocker", view)
	}
	clock.Add(int64(5*time.Hour + time.Second))
	dormantSnapshot(t, p)
	p.startDormantWindows()
	if calls.Load() != 2 {
		t.Fatal("new confirmed cycle not started", calls.Load())
	}
}

func TestWindowStartSkipsActiveUnverifiedUngrantedAndOtherProviders(t *testing.T) {
	for _, why := range []string{"active alias", "unverified", "not granted", "codex", "disabled", "stale", "missing ledger"} {
		t.Run(why, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			host := windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
				t.Error("ineligible inference")
				return nil, errors.New("unexpected")
			})
			p := windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, host)
			dormantSnapshot(t, p)
			switch why {
			case "active alias":
				p.requests["busy"] = attempt{Auth: "three"}
			case "unverified", "codex":
				for id, c := range p.creds {
					if why == "unverified" {
						c.AccountVerified = false
					} else {
						c.Provider = "codex"
					}
					p.creds[id] = c
				}
			case "not granted":
				p.cfg.ClientAccess = &ClientAccess{}
			case "disabled":
				p.cfg.QuotaWindowStart.Enabled = false
			case "stale":
				clock.Add(int64(windowstart.MaxEvidenceAge))
			case "missing ledger":
				_ = p.windowStartDB.Close()
			}
			p.startDormantWindows()
		})
	}
}

func TestWindowStartAmbiguousCallbackNeverClaimsNonDispatch(t *testing.T) {
	for _, response := range []string{`{}`, `{"version":1}`, `{"version":1,"dispatched":false}`, `{"version":1,"status_code":0}`, `{"version":1,"dispatched":false,"status_code":200}`, `not-json`} {
		t.Run(response, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			p := windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) { return []byte(response), nil }))
			dormantSnapshot(t, p)
			p.startDormantWindows()
			a, err := p.windowStarts.Read(p.creds["one"].Account)
			if err != nil || a.State != "unconfirmed" || !a.RetryAt.IsZero() {
				t.Fatal(a, err)
			}
		})
	}
}

func TestWindowStartRetriesReceived429WithFreshEvidenceAndHonorsSpendCaps(t *testing.T) {
	for _, reason := range []string{"rate_limit_error", "response_incomplete", "spend_limit_reached"} {
		t.Run(reason, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			var calls int
			p := windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
				calls++
				return json.Marshal(pluginapi.HostQuotaWindowStartResponse{Version: 1, Dispatched: true, StatusCode: 429, Reason: reason, Headers: http.Header{"Retry-After": {"3600"}}})
			}))
			dormantSnapshot(t, p)
			p.startDormantWindows()
			a, err := p.windowStarts.Read(p.creds["one"].Account)
			if err != nil || calls != 1 || a.HTTPStatus != 429 || a.Attempts != 1 {
				t.Fatal(a, calls, err)
			}
			if reason == "spend_limit_reached" {
				clock.Add(int64(24 * time.Hour))
				dormantSnapshot(t, p)
				p.startDormantWindows()
				if calls != 1 || a.State != "blocked" || !a.RetryAt.IsZero() {
					t.Fatal("spend cap retried", a, calls)
				}
				return
			}
			if a.State != "retry_wait" || a.RetryAt.Sub(a.Finished) != time.Hour {
				t.Fatal("Retry-After was lost", a)
			}
			clock.Add(int64(time.Hour - time.Second))
			dormantSnapshot(t, p)
			p.startDormantWindows()
			if calls != 1 {
				t.Fatal("retried before deadline")
			}
			clock.Add(int64(windowstart.MaxEvidenceAge))
			p.startDormantWindows()
			if calls != 1 {
				t.Fatal("retried with stale quota")
			}
			dormantSnapshot(t, p)
			p.requests["active-alias"] = attempt{Auth: "two"}
			p.startDormantWindows()
			if calls != 1 {
				t.Fatal("retried while an alias was busy")
			}
			delete(p.requests, "active-alias")
			p.startDormantWindows()
			if calls != 2 {
				t.Fatal("did not retry with fresh eligible evidence", calls)
			}
		})
	}
}

func TestWindowStartQuiesceCancelsPendingOperation(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	entered, cancelled, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	base := windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
		close(entered)
		<-cancelled
		return nil, errors.New("cancelled")
	})
	p := windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, func(method string, input any) (json.RawMessage, error) {
		if method == "host.http.cancel" {
			once.Do(func() { close(cancelled) })
		}
		return base(method, input)
	})
	dormantSnapshot(t, p)
	go func() { p.startDormantWindows(); close(done) }()
	<-entered
	invoke(t, p, pluginabi.MethodPluginQuiesce, map[string]any{})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("quiesce did not cancel the pending request")
	}
	a, err := p.windowStarts.Read(p.creds["one"].Account)
	if err != nil || a.State != "unconfirmed" {
		t.Fatal("cancellation must remain ambiguous", a, err)
	}
}

func TestWindowStartRechecksEvidenceAfterOpeningOperation(t *testing.T) {
	for _, changed := range []string{"paid usage", "weekly exhausted", "active window"} {
		t.Run(changed, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			var p *Plugin
			base := windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
				t.Error("updated quota evidence was ignored")
				return nil, errors.New("unexpected")
			})
			host := func(method string, input any) (json.RawMessage, error) {
				if method == "host.http.operation_open" {
					clock.Add(int64(time.Second))
					paid, week, reset := false, 20, "null"
					switch changed {
					case "paid usage":
						paid = true
					case "weekly exhausted":
						week = 100
					case "active window":
						reset = strconv.Quote(p.now().Add(5 * time.Hour).Format(time.RFC3339))
					}
					body := []byte(fmt.Sprintf(`{"five_hour":{"utilization":0,"resets_at":%s},"seven_day":{"utilization":%d,"resets_at":%q},"extra_usage":{"is_enabled":%v}}`, reset, week, p.now().Add(7*24*time.Hour).Format(time.RFC3339), paid))
					windows, err := observation.API("claude", body, p.now())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := p.credits.ObserveCredits(p.creds["one"].Account, observation.ClaudeExtraUsage(body, p.now())); err != nil {
						t.Fatal(err)
					}
					p.storeObservations(p.creds["one"], windows)
				}
				return base(method, input)
			}
			p = windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, host)
			dormantSnapshot(t, p)
			p.startDormantWindows()
			a, err := p.windowStarts.Read(p.creds["one"].Account)
			if err != nil || a.State != "idle" {
				t.Fatal("claim survived disqualifying evidence", a, err)
			}
		})
	}
}

func TestWindowStartSeesOrdinaryInferenceWithoutSessionOrQuotaDeclaration(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	p := windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
		t.Error("ordinary active inference was invisible")
		return nil, errors.New("unexpected")
	}))
	dormantSnapshot(t, p)
	q := pluginapi.RequestInterceptRequest{RequestID: "ordinary", Model: "claude-opus-5-5", Metadata: map[string]any{"selected_auth_id": "three"}}
	before := invoke(t, p, pluginabi.MethodRequestInterceptBefore, q).(pluginapi.RequestInterceptResponse)
	if before.Terminate {
		t.Fatal("before refused")
	}
	after := invoke(t, p, pluginabi.MethodRequestInterceptAfter, q).(pluginapi.RequestInterceptResponse)
	if after.Terminate {
		t.Fatal("after refused", string(after.ResponseBody))
	}
	if p.requests[q.RequestID].Session != "" || p.requests[q.RequestID].Client.Quota {
		t.Fatal("fixture unexpectedly has a session or quota declaration")
	}
	p.startDormantWindows()
	invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: q.RequestID})
}

func TestWindowStartObservesCapturedAccountWhenCredentialIsReplaced(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	var p *Plugin
	host := windowHost(func(pluginapi.HostQuotaWindowStartRequest) (json.RawMessage, error) {
		var captured int
		if err := p.db.QueryRow("SELECT count(*) FROM pacer_usage_credentials WHERE request_id=? AND auth=?", pluginapi.QuotaWindowStartRequestID("fixture-operation"), "one").Scan(&captured); err != nil || captured != 1 {
			t.Fatal("usage identity was not captured before dispatch", captured, err)
		}
		clock.Add(int64(time.Second))
		p.mu.Lock()
		c := p.creds["one"]
		c.Account = "replacement-account"
		p.creds["one"] = c
		p.mu.Unlock()
		return json.Marshal(pluginapi.HostQuotaWindowStartResponse{Version: 1, Dispatched: true, StatusCode: 200,
			Headers: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.001"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(p.now().Add(5*time.Hour).Unix(), 10)}}})
	})
	p = windowPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"), &clock, host)
	dormantSnapshot(t, p)
	original := p.creds["one"]
	p.startDormantWindows()
	if windows, err := p.observer.ReadWindows("replacement-account"); err != nil || len(windows) != 0 {
		t.Fatal("response evidence crossed account identities", windows, err)
	}
	if a, err := p.windowStarts.Read(original.Account); err != nil || a.State != "confirmed" {
		t.Fatal("original account was not confirmed", a, err)
	}
}
