package pacer

import (
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestLiveModePersistsWithoutOverridingExplicitYAMLChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	r := p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"dry-run"}`)})
	if r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	p.Close()
	p = startPlugin(t, path)
	defer p.Close()
	if p.cfg.Routing.Mode != "dry-run" {
		t.Fatal("saved mode lost on restart")
	}
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\nrouting:\n  mode: off\n")})
	if p.cfg.Routing.Mode != "dry-run" {
		t.Fatal("unrelated reload erased management choice")
	}
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\nrouting:\n  mode: dry-run\n")})
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\nrouting:\n  mode: off\n")})
	if p.cfg.Routing.Mode != "off" {
		t.Fatal("explicit YAML change ignored")
	}
	r = p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"dry-run"}{}`)})
	if r.StatusCode < 400 || p.cfg.Routing.Mode != "off" {
		t.Fatal("trailing input accepted")
	}
}
