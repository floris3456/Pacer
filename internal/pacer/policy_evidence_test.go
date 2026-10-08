package pacer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestEffectiveTTLReadbackRequiresPersistedNativeEvidenceAndAuthorizedGet(t *testing.T) {
	now := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cachepolicy.Create(context.Background(), db, cachepolicy.Options{Clock: clock, Routes: []cachepolicy.Route{{ProviderRoute: "claude-main", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	scope := pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "trusted-plugin", SessionID: "chat", ProviderRoute: "claude-main"}
	config := "state_path: " + path + "\ncache_policy:\n  enabled: true\n  routes:\n    - id: claude-main\n      models: [claude-opus-5-5]\n  trusted_empty_installations:\n    - caller_digest: " + scope.CallerScope + "\n      installation_id: trusted-plugin\n"
	start := func() *Plugin {
		p := New(testHost)
		p.now = clock
		invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte(config), "schema_version": 7})
		return p
	}
	p := start()
	defer func() { p.Close() }()
	command := pluginapi.CachePolicyRequest{Scope: scope, Capability: strings.Repeat("a", 42) + "A", Method: "POST", Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "ensure", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute, ContextEpoch: "empty", TTL: "1h", OperationID: "enroll", Enrollment: "new_empty"}}
	original := invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	if original.StatusCode != 200 {
		t.Fatal(string(original.Body))
	}
	get := pluginapi.CachePolicyRequest{Scope: scope, Capability: command.Capability, Method: "GET", Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "read", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute}}
	read := func() map[string]any {
		t.Helper()
		r := invoke(t, p, pluginabi.MethodCachePolicyHandle, get).(pluginapi.CachePolicyResponse)
		var data map[string]any
		if r.StatusCode != 200 || json.Unmarshal(r.Body, &data) != nil {
			t.Fatal(r.StatusCode, string(r.Body))
		}
		return data
	}
	if got := read(); got["last_effective_ttl"] != nil {
		t.Fatal("selection without dispatch fabricated effective TTL", got)
	}
	selected := invoke(t, p, pluginabi.MethodCachePolicyResolve, pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: scope, Capability: command.Capability, ContextEpoch: "empty", Revision: 1, Kind: "primary", RequestID: "logical", Model: "claude-opus-5-5", SourceFormat: "claude"}).(pluginapi.CachePolicy)
	c := p.creds["a"]
	if err = p.captureUsageCredential("logical", c); err != nil {
		t.Fatal(err)
	}
	usage := map[string]any{"Provider": "claude", "Model": "claude-opus-5-5", "LifecycleRequestID": "logical", "RequestID": "physical", "AuthID": c.ID, "AuthIndex": c.Index, "ExecutorType": "ClaudeExecutor", "SessionID": "pck:independent-affinity-topology", "NodeKind": "primary", "RequestedAt": now.Add(-2 * time.Second), "Latency": int64(time.Second), "Failed": false, "AccountingVersion": 2, "Detail": map[string]any{}, "CachePolicy": map[string]any{"selected": selected, "effective_ttl": "1h", "breakpoint_count": 2, "dispatched": true, "upstream_started_at": now.Add(-1500 * time.Millisecond)}}
	usage["SourceEvidence"] = map[string]any{"scope": map[string]any{"provider": "claude", "auth_id": c.ID, "auth_index": c.Index, "kind": "cpa.claude.http.first-party", "version": 1}, "manager_incarnation_id": "host-incarnation", "tracked": true, "requested_at": now.Add(-2 * time.Second), "upstream_started_at": now.Add(-1490 * time.Millisecond), "final_usage_at": now.Add(-900 * time.Millisecond)}
	raw, _ := json.Marshal(usage)
	if err = p.recordUsage(raw); err != nil {
		t.Fatal(err)
	}
	assertRead := func() {
		t.Helper()
		got := read()
		if got["last_effective_ttl"] != "1h" {
			t.Fatal("persisted marker evidence missing", got)
		}
		for _, key := range []string{"cache_refresh_at", "protected_until", "effective_ttl", "caller_scope", "capability"} {
			if got[key] != nil {
				t.Fatal("readback overclaimed or leaked", key)
			}
		}
	}
	assertRead()
	retry := invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	if retry.StatusCode != 200 || !bytes.Equal(retry.Body, original.Body) {
		t.Fatal("live telemetry changed idempotent POST receipt")
	}
	for _, wrong := range []pluginapi.CachePolicyRequest{
		{Scope: scope, Capability: strings.Repeat("b", 42) + "A", Method: "GET"},
		{Scope: pluginapi.CachePolicyScope{CallerScope: strings.Repeat("b", 64), InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute}, Capability: command.Capability, Method: "GET"},
	} {
		wrong.Operation = get.Operation
		r := invoke(t, p, pluginabi.MethodCachePolicyHandle, wrong).(pluginapi.CachePolicyResponse)
		if r.StatusCode == 200 || bytes.Contains(r.Body, []byte("last_effective_ttl")) {
			t.Fatal("readback bypassed capability/caller authorization", r.StatusCode)
		}
	}
	usage["Failed"] = true
	bad, _ := json.Marshal(usage)
	if err = p.recordUsage(bad); err == nil {
		t.Fatal("conflicting authoritative event accepted")
	}
	assertRead()
	invoke(t, p, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "logical", Outcome: pluginapi.RequestCompletionRejected})
	p.Close()
	p = start()
	assertRead()
	if err = p.recordUsage(raw); err != nil {
		t.Fatal("authoritative duplicate projection failed", err)
	}
	assertRead()
	p.refreshMeasurements()
	for _, r := range p.measurements {
		if r.Coverage.Observed {
			t.Fatal("marker TTL readback invented delivery coverage")
		}
	}
}
