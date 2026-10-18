package pacer

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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

func patchSettings(t *testing.T, p *Plugin, body string) pluginapi.ManagementResponse {
	t.Helper()
	return p.manage(pluginapi.ManagementRequest{Method: "PATCH", Path: "/v0/management/plugins/pacer/settings", Body: []byte(body)})
}

// Page settings apply at once, survive restarts and unrelated reloads, and give
// way to an explicit YAML change of that setting only.
func TestPageSettingsPersistUntilTheirYAMLSeedChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	p := startPlugin(t, path)
	// startPlugin's YAML has compact_on_switch: true; the rest are defaults.
	if s := p.settings; !s.CompactOnSwitch || s.Band != 90 || s.NewChatMinimum != 15 || s.WarmMinutes != 60 || s.TransientRetries != 3 || len(s.Providers) != 1 || s.routing().Slack != 48*time.Hour {
		t.Fatalf("defaults %+v", s)
	}
	r := patchSettings(t, p, `{"compact_on_switch":false,"compaction_models":["claude-sonnet-4-6","gemini-3.8-flash-high"],"band_percent":80,"warm_minutes":5,"paused_accounts":["claude:abc"],"due_slack_hours":36}`)
	if r.StatusCode != 200 || p.cfg.CompactOnSwitch || p.settings.Band != 80 || p.settings.routing().Band != .8 || !p.settings.paused("claude:abc") || p.settings.warmFor() != 5*time.Minute || p.settings.routing().Slack != 36*time.Hour {
		t.Fatal(r.StatusCode, string(r.Body))
	}
	var view struct {
		Changed []string `json:"changed"`
	}
	if json.Unmarshal(r.Body, &view) != nil || len(view.Changed) != 6 {
		t.Fatal(string(r.Body))
	}
	p.Close()
	p = startPlugin(t, path)
	defer p.Close()
	if p.cfg.CompactOnSwitch || len(p.settings.CompactionModels) != 2 || p.settings.CompactionModels[1] != "gemini-3.8-flash-high" || p.settings.WarmMinutes != 5 {
		t.Fatalf("lost on restart %+v", p.settings)
	}
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\ncompact_on_switch: true\nrouting:\n  mode: off\n")})
	if p.cfg.CompactOnSwitch || p.settings.Band != 80 {
		t.Fatal("unrelated reload erased page settings")
	}
	// An explicit YAML change of compact_on_switch wins; the others stay.
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\ncompact_on_switch: false\nrouting:\n  mode: off\n")})
	invoke(t, p, pluginabi.MethodPluginReconfigure, map[string]any{"config_yaml": []byte("state_path: " + path + "\ncompact_on_switch: true\nrouting:\n  mode: off\n")})
	if !p.cfg.CompactOnSwitch || p.settings.Band != 80 {
		t.Fatalf("YAML change: %+v", p.settings)
	}
}

func TestPageSettingsRefuseInvalidValues(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	before := p.settings
	for _, body := range []string{
		`{}`, `[]`, `{"mode":"on"}`, `{"band_percent":"80"}`, `{"band_percent":40}`, `{"band_percent":101}`,
		`{"new_chat_minimum_percent":-1}`, `{"short_floor_percent":60}`, `{"warm_minutes":1441}`, `{"warm_minutes":1.5}`,
		`{"transient_retries":11}`, `{"providers":[]}`, `{"providers":["gemini"]}`, `{"providers":["claude","claude"]}`,
		`{"compaction_models":["a","a"]}`, `{"compaction_models":[""]}`, `{"compaction_models":["bad\nname"]}`,
		`{"compaction_models":["1","2","3","4","5","6","7","8","9"]}`, `{"paused_accounts":[""]}`,
		`{"due_slack_hours":-1}`, `{"due_slack_hours":169}`,
	} {
		if r := patchSettings(t, p, body); r.StatusCode != 400 {
			t.Fatal("accepted", body, r.StatusCode)
		}
	}
	if !reflect.DeepEqual(p.settings, before) {
		t.Fatal("refused patch changed settings")
	}
	if r := patchSettings(t, p, `{"compaction_models":null,"providers":["claude","codex"]}`); r.StatusCode != 200 || p.settings.CompactionModels == nil || len(p.cfg.Routing.Providers) != 2 {
		t.Fatal(string(r.Body))
	}
}
