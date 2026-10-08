package pacer

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestDiscoveryDoesNotApplyFirstPartyObserverToCompatibleGateways(t *testing.T) {
	p := New(func(method string, input any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.Marshal(map[string]any{"files": []map[string]any{
				{"id": "first-party", "auth_index": "one", "provider": "claude"},
				{"id": "gateway", "auth_index": "two", "provider": "claude", "base_url": "https://unknown-gateway.invalid"},
				{"id": "port", "auth_index": "port", "provider": "claude", "base_url": "https://api.anthropic.com:8443"},
				{"id": "path", "auth_index": "path", "provider": "claude", "base_url": "https://api.anthropic.com/custom"},
				{"id": "query", "auth_index": "query", "provider": "claude", "base_url": "https://api.anthropic.com?endpoint=custom"},
				{"id": "user", "auth_index": "user", "provider": "claude", "base_url": "https://dummy@api.anthropic.com"},
				{"id": "codex-path", "auth_index": "codex-path", "provider": "codex", "base_url": "https://chatgpt.com/custom"},
				{"id": "other", "auth_index": "three", "provider": "openai"},
				{"id": "disabled", "auth_index": "four", "provider": "claude", "disabled": true},
			}})
		}
		return testHost(method, input)
	})
	defer p.Close()
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\n")})
	if len(p.creds) != 1 || p.creds["first-party"].ID == "" {
		t.Fatal(p.creds)
	}
	jobs, err := p.observer.Jobs()
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
}

func TestDiscoveryKeepsBillingMappingProvenance(t *testing.T) {
	p := New(func(method string, input any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []map[string]any{
				{"id": "claude", "auth_index": "known", "provider": "claude", "base_url": "https://api.anthropic.com/"},
				{"id": "claude-alias", "auth_index": "alias", "provider": "claude"},
				{"id": "fallback", "auth_index": "fallback", "provider": "claude"},
				{"id": "codex", "auth_index": "codex", "provider": "codex", "base_url": "https://chatgpt.com/backend-api/codex"},
			}})
		case "host.auth.get":
			switch input.(map[string]string)["auth_index"] {
			case "known", "alias":
				return json.RawMessage(`{"json":{"account_uuid":"same-billing-identity"}}`), nil
			case "codex":
				return json.RawMessage(`{"json":{"account_id":"codex-identity"}}`), nil
			default:
				return json.RawMessage(`{"json":{}}`), nil
			}
		}
		return testHost(method, input)
	})
	defer p.Close()
	invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\n")})
	if len(p.creds) != 4 || !p.creds["claude"].AccountVerified || !p.creds["claude-alias"].AccountVerified || !p.creds["codex"].AccountVerified || p.creds["fallback"].AccountVerified {
		t.Fatal("discovery lost verified-vs-fallback provenance", p.creds)
	}
	if p.creds["claude"].Account != p.creds["claude-alias"].Account || p.creds["claude"].Account == p.creds["fallback"].Account {
		t.Fatal("aliases mapped incorrectly")
	}
}
