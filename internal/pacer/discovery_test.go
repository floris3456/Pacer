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
