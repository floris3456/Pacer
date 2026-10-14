package pacer

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestLiveModePersistsWithoutOverridingExplicitYAMLChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	r := p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"on"}`)})
	if r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	p.Close()
	p = startPlugin(t, path)
	defer p.Close()
	if p.cfg.Routing.Mode != "on" {
		t.Fatal("saved mode lost on restart")
	}
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\nrouting:\n  mode: off\n")})
	if p.cfg.Routing.Mode != "on" {
		t.Fatal("unrelated reload erased management choice")
	}
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\nrouting:\n  mode: on\n")})
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\nrouting:\n  mode: off\n")})
	if p.cfg.Routing.Mode != "off" {
		t.Fatal("explicit YAML change ignored")
	}
	r = p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"on"}{}`)})
	if r.StatusCode < 400 || p.cfg.Routing.Mode != "off" {
		t.Fatal("trailing input accepted")
	}
	for _, mode := range []string{"dry-run", "active", "ON"} {
		if r = p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/routing", Body: []byte(`{"mode":"` + mode + `"}`)}); r.StatusCode != 400 || p.cfg.Routing.Mode != "off" {
			t.Fatal("mode accepted", mode, r.StatusCode)
		}
	}
}

// The former dry-run mode never routed. Its YAML and persisted settings read as
// off, so an existing installation loads unchanged.
func TestLegacyDryRunModeReadsAsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	if _, err := p.db.Exec(`UPDATE pacer_routing_settings SET configured='dry-run',effective='dry-run'`); err != nil {
		t.Fatal(err)
	}
	p.Close()
	for _, yaml := range []string{"routing:\n  mode: dry-run\n", "routing:\n  mode: off\n"} {
		p = New(testHost)
		invoke(t, p, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("state_path: " + path + "\n" + yaml)})
		if p.cfg.Routing.Mode != "off" {
			t.Fatal(yaml, p.cfg.Routing.Mode)
		}
		p.Close()
	}
	for _, yaml := range []string{"routing:\n  mode: active\n", "routing_eligible: true\n"} {
		p = New(testHost)
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("state_path: " + path + "\n" + yaml)})
		if _, err := p.Handle(pluginabi.MethodPluginRegister, raw); err == nil {
			t.Fatal("accepted", yaml)
		}
		p.Close()
	}
}
