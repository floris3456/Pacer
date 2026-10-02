package bridge

import (
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshUsesHostCredentialsAndDeduplicatesAccounts(t *testing.T) {
	cfg := handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), Credentials: []handoff.Credential{
		{ID: "a", Index: "a", Provider: "claude", Account: "first-name", Alias: "A"},
		{ID: "b", Index: "b", Provider: "claude", Account: "other-name", Alias: "B"},
	}}
	e, err := handoff.Open(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	host := func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.get_runtime":
			id := payload.(map[string]string)["auth_index"]
			data, _ := json.Marshal(pluginapi.HostAuthGetRuntimeResponse{Auth: pluginapi.HostAuthFileEntry{ID: id}})
			return data, nil
		case "host.auth.get":
			return json.RawMessage(`{"json":{"access_token":"fixture-token","account_uuid":"same-provider-account"}}`), nil
		case "host.http.operation_open":
			return json.RawMessage(`{"operation_id":"fixture"}`), nil
		case "host.http.do":
			calls++
			if payload.(map[string]any)["url"] != "https://api.anthropic.com/api/oauth/usage" {
				t.Fatal("unapproved quota URL")
			}
			body := []byte(fmt.Sprintf(`{"five_hour":{"utilization":0,"resets_at":%q},"seven_day":{"utilization":0,"resets_at":%q}}`, time.Now().Add(5*time.Hour).UTC().Format(time.RFC3339), time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339)))
			data, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: body})
			return data, nil
		}
		return nil, fmt.Errorf("unexpected callback %s", method)
	}
	p := New(host)
	p.Engine = e
	defer p.Close()
	p.refresh(nil)
	a, _ := e.Credential("a")
	b, _ := e.Credential("b")
	if a.Account != b.Account || a.Account == "same-provider-account" || calls != 1 {
		t.Fatal("provider account was not safely deduplicated", calls)
	}
	p.refresh(nil)
	if calls != 1 {
		t.Fatal("quota API cadence not bounded", calls)
	}
	state, err := e.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-token", "same-provider-account"} {
		if strings.Contains(string(state), secret) {
			t.Fatal("plaintext credential material was persisted")
		}
	}
}

func TestReconfigurePreservesStateAndCloseDrainsRefresh(t *testing.T) {
	cfg := handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), Credentials: []handoff.Credential{
		{ID: "a", Index: "a", Provider: "claude", Account: "a", Alias: "A"},
	}}
	entered, release, refreshed, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	p := New(func(method string, payload any) (json.RawMessage, error) {
		close(entered)
		<-release
		return nil, fmt.Errorf("provider unavailable")
	})
	register := func(method string) error {
		config, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		request, err := json.Marshal(map[string][]byte{"config_yaml": config})
		if err != nil {
			return err
		}
		_, err = p.Handle(method, request)
		return err
	}
	if err := register(pluginabi.MethodPluginRegister); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	engine := p.Engine
	before, _ := engine.MarshalState()
	if err := register(pluginabi.MethodPluginReconfigure); err != nil || engine != p.Engine {
		t.Fatal("same config reconfigure replaced state", err)
	}
	cfg.MaxAge = 120
	if err := register(pluginabi.MethodPluginReconfigure); err == nil {
		t.Fatal("live configuration change was accepted")
	}
	after, _ := engine.MarshalState()
	if string(before) != string(after) {
		t.Fatal("refused reconfigure mutated durable state")
	}
	go func() { p.refresh(nil); close(refreshed) }()
	<-entered
	go func() { p.Close(); close(closed) }()
	for !p.stopped.Load() {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		t.Fatal("closed state while refresh callback was running")
	default:
	}
	close(release)
	<-refreshed
	<-closed
	// A stopped plugin must not launch another host quota request.
	p.refresh(nil)
	p.Close()
}
