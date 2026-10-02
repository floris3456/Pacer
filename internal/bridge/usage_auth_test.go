package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
)

func TestUsageBeforeAnyRequestAuthenticatesWithoutEnrollment(t *testing.T) {
	var revoked atomic.Bool
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/models" || r.Method != "GET" {
			t.Error("unexpected auth request", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-key" || revoked.Load() {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer server.Close()
	p := New(nil)
	p.usageAuthURL = server.URL + "/v1/models"
	var err error
	p.Engine, err = handoff.Open(handoff.Config{StatePath: filepath.Join(t.TempDir(), "usage.sqlite"), Credentials: []handoff.Credential{
		{ID: "claude-a", Index: "1", Account: "a", Provider: "claude", Alias: "Claude A"},
		{ID: "codex-b", Index: "2", Account: "b", Provider: "codex", Alias: "Codex B"},
	}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	before, _ := p.Engine.MarshalState()
	query := func(key, all string) pluginapi.ManagementResponse {
		return p.usage(pluginapi.ManagementRequest{Headers: http.Header{"Authorization": {"Bearer " + key}}, Query: url.Values{"auth": {"api-key"}, "all": {all}}})
	}
	if res := query("fixture-key", "true"); res.StatusCode != 200 {
		t.Fatal(res)
	} else {
		var result handoff.Usage
		if err := json.Unmarshal(res.Body, &result); err != nil {
			t.Fatal(err)
		}
		if !result.All || len(result.Credentials) != 2 || result.Current != "" || len(result.Active) != 0 || result.LastUsed != 0 {
			t.Fatal(result)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("must verify that CPA requires authentication and accepts the supplied key")
	}
	if res := query("wrong-key", "true"); res.StatusCode != 401 {
		t.Fatal("invalid key accepted", res)
	}
	revoked.Store(true)
	if res := query("fixture-key", "true"); res.StatusCode != 401 {
		t.Fatal("revoked key was cached", res)
	}
	if res := query("fixture-key", "false"); res.StatusCode != 400 {
		t.Fatal("unbound model scope accepted", res)
	}
	after, _ := p.Engine.MarshalState()
	if string(before) != string(after) {
		t.Fatal("all-usage read enrolled or changed a chat")
	}
}

func TestUsageAuthenticationRefusesOpenEndpointsRedirectsAndRemoteURLs(t *testing.T) {
	for _, raw := range []string{"https://example.com/v1/models", "http://localhost:8317/v1/models", "http://127.0.0.1:8317/v0/management/models", "http://key@127.0.0.1:8317/v1/models", "http://127.0.0.1:8317/v1/models?key=secret"} {
		if _, err := validateUsageAuthURL(raw); err == nil {
			t.Fatal("invalid auth target accepted", raw)
		}
	}
	var trapped atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trapped.Add(1)
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer trap.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		http.Redirect(w, r, trap.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	for _, origin := range []string{trap.URL, redirect.URL} {
		p := New(nil)
		p.usageAuthURL = origin + "/v1/models"
		ok, err := p.authenticateUsageKey("fixture-key")
		if ok || err == nil {
			t.Fatal("unsafe authentication endpoint accepted", origin)
		}
	}
	if trapped.Load() != 1 {
		t.Fatal("redirect forwarded credential to trap", trapped.Load())
	}
}
