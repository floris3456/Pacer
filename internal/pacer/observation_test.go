package pacer

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPassiveHeadersAndManagementReadsNeverMultiplyQuotaCalls(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var calls atomic.Int64
	host := func(method string, input any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.Marshal(map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "ia", "provider": "claude"}}})
		}
		if method == "host.http.do" {
			calls.Add(1)
		}
		return testHost(method, input)
	}
	p := New(host)
	p.now = now
	defer p.Close()
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\n")})
	short, long := now().Add(5*time.Hour).Unix(), now().Add(7*24*time.Hour).Unix()
	for i := 0; i < 24; i++ {
		headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(short, 10)}, "Anthropic-Ratelimit-Unified-7d-Utilization": {"0.3"}, "Anthropic-Ratelimit-Unified-7d-Reset": {strconv.FormatInt(long, 10)}}
		invoke(t, p, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{Metadata: map[string]any{"selected_auth_id": "a"}, ResponseHeaders: headers})
		for j := 0; j < 20; j++ {
			response := invoke(t, p, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/pacer/state"}).(pluginapi.ManagementResponse)
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
		}
		p.reconcile()
		clock.Add(int64(5 * time.Minute))
	}
	if calls.Load() != 0 {
		t.Fatalf("fresh passive signals caused %d provider calls", calls.Load())
	}
	rows, err := p.observer.ReadWindows(p.creds["a"].Account)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
}

func TestAdapterPreservesSharedRetryAfterAcrossPluginRestart(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var calls atomic.Int64
	host := func(method string, input any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "ia", "provider": "claude"}, {"id": "alias-a", "auth_index": "ialias", "provider": "claude"}}})
		case "host.auth.get":
			return json.Marshal(map[string]any{"json": map[string]string{"account_uuid": "same-account", "access_token": "dummy-fixture-token"}})
		case "host.auth.get_runtime":
			id := "a"
			if input.(map[string]string)["auth_index"] == "ialias" {
				id = "alias-a"
			}
			return json.Marshal(pluginapi.HostAuthGetRuntimeResponse{Auth: pluginapi.HostAuthFileEntry{ID: id}})
		case "host.http.operation_open":
			return json.Marshal(map[string]string{"operation_id": "owned-fixture-operation"})
		case "host.http.do":
			calls.Add(1)
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"46800"}}})
		}
		return json.Marshal(map[string]any{})
	}
	path := filepath.Join(t.TempDir(), "state.sqlite")
	open := func() *Plugin {
		p := New(host)
		p.now = now
		invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: " + path + "\n")})
		return p
	}
	p := open()
	clock.Add(int64(6 * time.Minute))
	p.reconcile()
	if calls.Load() != 1 {
		t.Fatal("aliases did not share request", calls.Load())
	}
	job, _, err := p.observer.Status(p.creds["a"].Account)
	if err != nil || job.EndpointNotBefore.Before(now().Add(13*time.Hour)) {
		t.Fatal(job, err)
	}
	p.Close()
	p = open()
	defer p.Close()
	clock.Add(int64(7 * time.Hour))
	for i := 0; i < 20; i++ {
		p.reconcile()
	}
	if calls.Load() != 1 {
		t.Fatal("restart shortened provider Retry-After", calls.Load())
	}
	rows, err := p.observer.ReadWindows(p.creds["a"].Account)
	if err != nil || len(rows) != 0 {
		t.Fatal("analytics429 became inference exhaustion", rows, err)
	}
}
