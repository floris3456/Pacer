package bridge

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

func TestSchedulerAcceptsCPAProviderList(t *testing.T) {
	p := New(nil)
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "state.sqlite"), Credentials: []handoff.Credential{
		{ID: "small", Index: "1", Provider: "claude", Account: "small", Alias: "Small", Rank: 1},
	}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Now().Unix()
	if err = p.Engine.Observe("small", []quota.Window{
		{Scope: "all", Seconds: 18000, Remaining: 100, Reset: now + 18000, Observed: now},
		{Scope: "all", Seconds: 604800, Remaining: 100, Reset: now + 604800, Observed: now},
	}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []pluginapi.SchedulerPickRequest{
		{Provider: "claude", Model: "claude-sonnet", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "small", Provider: "claude"}}},
		{Providers: []string{"claude"}, Model: "claude-sonnet", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "small", Provider: "claude"}}},
	} {
		raw, _ := json.Marshal(request)
		result, err := p.Handle(pluginabi.MethodSchedulerPick, raw)
		if err != nil {
			t.Fatal(err)
		}
		pick := result.(pluginapi.SchedulerPickResponse)
		if !pick.Handled || pick.Reject || pick.AuthID != "small" {
			t.Fatal(pick)
		}
	}
	// A multi-provider alias must not bypass held accounts by falling through
	// to the host scheduler; unconfigured provider pools still delegate normally.
	for _, tc := range []struct {
		providers []string
		handled   bool
	}{
		{[]string{"claude", "codex"}, true}, {[]string{"codex", "antigravity"}, false},
	} {
		request := pluginapi.SchedulerPickRequest{Providers: tc.providers, Model: "alias"}
		for _, provider := range tc.providers {
			request.Candidates = append(request.Candidates, pluginapi.SchedulerAuthCandidate{ID: provider, Provider: provider})
		}
		raw, _ := json.Marshal(request)
		result, err := p.Handle(pluginabi.MethodSchedulerPick, raw)
		if err != nil {
			t.Fatal(err)
		}
		pick := result.(pluginapi.SchedulerPickResponse)
		if pick.Handled != tc.handled || (tc.handled && (!pick.Reject || pick.RejectCode != "quota_provider_ambiguous")) {
			t.Fatal(pick)
		}
	}
}
