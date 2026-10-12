package pacer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestExplicitClientGrantsRequireHostEnforcement(t *testing.T) {
	guard := requiredInterceptors{Version: 1, PluginID: "pacer", Required: true, RequestInterceptors: true, NestedModelInterceptors: true, RetryInterceptors: true, AuthPoolFiltering: true}
	for _, test := range []struct {
		name string
		edit func(*requiredInterceptors)
		ok   bool
	}{
		{"enforced", func(*requiredInterceptors) {}, true},
		{"older-host", func(g *requiredInterceptors) { *g = requiredInterceptors{} }, false},
		{"unknown-version", func(g *requiredInterceptors) { g.Version = 2 }, false},
		{"other-plugin", func(g *requiredInterceptors) { g.PluginID = "another" }, false},
		{"optional", func(g *requiredInterceptors) { g.Required = false }, false},
		{"no-request-guard", func(g *requiredInterceptors) { g.RequestInterceptors = false }, false},
		{"nested-bypass", func(g *requiredInterceptors) { g.NestedModelInterceptors = false }, false},
		{"retry-bypass", func(g *requiredInterceptors) { g.RetryInterceptors = false }, false},
		{"no-pool-filter", func(g *requiredInterceptors) { g.AuthPoolFiltering = false }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := New(testHost)
			defer p.Close()
			g := guard
			test.edit(&g)
			config := "enabled: true\nstate_path: " + filepath.Join(t.TempDir(), "state.sqlite") + "\nclient_access:\n  grants: []\n"
			raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config), "schema_version": 7, "host_capabilities": map[string]any{"required_interceptors": g}})
			_, err := p.Handle(pluginabi.MethodPluginRegister, raw)
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				p.mu.Lock()
				api := p.clientAPILocked()
				p.mu.Unlock()
				if api.Access["enforcement"] != "required-interceptors" || !api.Features["account_access"].Enabled {
					t.Fatalf("required enforcement not advertised: %+v", api)
				}
				// Reconfigure cannot silently weaken the host guarantee.
				raw, _ = json.Marshal(map[string]any{"config_yaml": []byte(config), "schema_version": 7})
				if _, err = p.configure(raw); err == nil || !p.hostGuard.enforced() {
					t.Fatal("host guard removed from active granted configuration")
				}
			} else if err == nil || !strings.Contains(err.Error(), "required-interceptor") {
				t.Fatalf("unsafe registration returned %v", err)
			}
		})
	}
}
