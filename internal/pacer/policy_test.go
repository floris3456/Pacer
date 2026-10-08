package pacer

import (
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

func TestNativePolicyRequiresExplicitInitializationAndTrustedInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	config := "state_path: " + path + "\ncache_policy:\n  enabled: true\n  routes:\n    - id: claude-main\n      models: [claude-opus-5-5]\n"
	reg := func(p *Plugin) (any, error) {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config), "schema_version": 7})
		return p.Handle(pluginabi.MethodPluginRegister, raw)
	}
	p := New(testHost)
	if _, err := reg(p); err == nil {
		t.Fatal("missing state initialized implicitly")
	}
	p.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cachepolicy.Create(context.Background(), db, cachepolicy.Options{Clock: func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) }, Routes: []cachepolicy.Route{{ProviderRoute: "claude-main", TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}}}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	p = New(testHost)
	if _, err = reg(p); err != nil {
		t.Fatal(err)
	}
	scope := pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "trusted-plugin", SessionID: "chat", ProviderRoute: "claude-main"}
	command := pluginapi.CachePolicyRequest{Scope: scope, Capability: strings.Repeat("a", 42) + "A", Method: "POST", Operation: pluginapi.CachePolicyOperation{Protocol: 1, Operation: "ensure", InstallationID: scope.InstallationID, SessionID: scope.SessionID, ProviderRoute: scope.ProviderRoute, ContextEpoch: "empty", TTL: "1h", OperationID: "ensure", Enrollment: "new_empty"}}
	response := invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	if response.StatusCode != 422 {
		t.Fatal("untrusted empty enrollment accepted", response.StatusCode, string(response.Body))
	}
	p.Close()
	config += "  trusted_empty_installations:\n    - caller_digest: " + scope.CallerScope + "\n      installation_id: trusted-plugin\n"
	p = New(testHost)
	defer p.Close()
	if _, err = reg(p); err != nil {
		t.Fatal(err)
	}
	command.Operation.OperationID = "trusted-ensure"
	response = invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode, string(response.Body))
	}
	command.Scope.CallerScope = strings.Repeat("b", 64)
	command.Scope.SessionID = "another"
	command.Operation.SessionID = "another"
	response = invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	if response.StatusCode != 422 {
		t.Fatal("foreign caller reused installation authority", response.StatusCode)
	}
	command.Scope.CallerScope = scope.CallerScope
	command.Scope.InstallationID = "untrusted"
	command.Operation.InstallationID = "untrusted"
	command.Operation.OperationID = "different-installation"
	response = invoke(t, p, pluginabi.MethodCachePolicyHandle, command).(pluginapi.CachePolicyResponse)
	if response.StatusCode != 422 {
		t.Fatal("foreign installation reused caller authority", response.StatusCode)
	}
}
