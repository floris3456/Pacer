package pacer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageAuthenticationValidatesBoundaryWithoutRedirectOrAnonymousFallback(t *testing.T) {
	for _, mode := range []string{"protected", "open", "redirect", "not-models"} {
		t.Run(mode, func(t *testing.T) {
			hits := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if mode == "redirect" {
					http.Redirect(w, r, "/elsewhere", 302)
					return
				}
				if mode != "open" && r.Header.Get("Authorization") != "Bearer fixture-key" {
					w.WriteHeader(401)
					return
				}
				if mode == "not-models" {
					w.Write([]byte(`{}`))
					return
				}
				w.Write([]byte(`{"object":"list","data":[]}`))
			}))
			defer s.Close()
			endpoint, err := validateUsageURL(s.URL + "/v1/models")
			if err != nil {
				t.Fatal(err)
			}
			ok, err := authenticateUsage(endpoint, "fixture-key")
			if mode == "protected" {
				if !ok || err != nil {
					t.Fatal(ok, err)
				}
				ok, err = authenticateUsage(endpoint, "bad-key")
				if ok || err != nil {
					t.Fatal(ok, err)
				}
			} else if ok || err == nil {
				t.Fatal("unsafe authentication accepted", mode, ok, err)
			}
			if mode == "redirect" && hits != 1 {
				t.Fatal("followed redirect", hits)
			}
		})
	}
	for _, raw := range []string{"https://127.0.0.1:8317/v1/models", "http://localhost:8317/v1/models", "http://127.0.0.1:8317/v1/models?key=x", "http://key@127.0.0.1:8317/v1/models", "http://example.com:8317/v1/models", "http://127.0.0.1/v1/models"} {
		if _, err := validateUsageURL(raw); err == nil {
			t.Fatal("unsafe URL accepted", raw)
		}
	}
}

func TestCompatibilityDiscoveryCapabilityScopeAndUncertainWindows(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	request := pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/pacer/events", Query: url.Values{}}
	r := p.events(request)
	var discovery map[string]any
	json.Unmarshal(r.Body, &discovery)
	if r.StatusCode != 200 || discovery["mode"] != "off" || discovery["protocol"] != float64(1) {
		t.Fatal(string(r.Body))
	}
	token := strings.Repeat("c", 64)
	q := tracker.Claim{Protocol: 1, Installation: "install", Session: "session", Route: "claude/opus", Request: "request", Kind: "primary", Capability: tracker.Digest(token)}
	c := p.creds["a"]
	_, err := p.chats.Admit("id", tracker.Key("caller", "claude", q), tracker.Selection{Auth: c.ID, Account: c.Account, Provider: c.Provider, Alias: c.Alias, Model: "claude-opus-5-5"}, &q, true)
	if err != nil {
		t.Fatal(err)
	}
	at := p.now()
	remaining := 75.0
	valid := observation.Window{Provider: "claude", Scope: "all", Seconds: 18000, Remaining: &remaining, Reset: at.Add(time.Hour), Usage: observation.Field{At: at, Source: "header"}, ResetEvidence: observation.Field{At: at, Source: "header", Quality: "absolute"}}
	partial := valid
	partial.Seconds = 604800
	partial.Reset = time.Time{}
	partial.ResetEvidence = observation.Field{}
	expired := valid
	expired.Scope = "opus"
	expired.Reset = at.Add(-time.Minute)
	if err = p.observer.Observe(c.Account, c.Provider, []observation.Window{valid, partial, expired}); err != nil {
		t.Fatal(err)
	}
	request.Headers = http.Header{"Authorization": {"Bearer " + token}}
	request.Query.Set("model", q.Route)
	r = p.events(request)
	var snapshot struct {
		Protocol    int              `json:"protocol"`
		Mode        string           `json:"mode"`
		PrimaryUsed bool             `json:"primary_used"`
		Windows     []map[string]any `json:"windows"`
		Events      []any            `json:"events"`
	}
	if err = json.Unmarshal(r.Body, &snapshot); err != nil || r.StatusCode != 200 || snapshot.Protocol != 1 || snapshot.Mode != "off" || !snapshot.PrimaryUsed || len(snapshot.Windows) != 1 || snapshot.Events == nil {
		t.Fatal(string(r.Body), err)
	}
	for _, field := range []string{"remaining", "seconds", "reset"} {
		if _, ok := snapshot.Windows[0][field].(float64); !ok {
			t.Fatal("legacy numeric field missing", field)
		}
	}
	request.Query.Set("model", "another/route")
	if p.events(request).StatusCode != 401 {
		t.Fatal("capability crossed route scope")
	}
	request.Query.Set("model", q.Route)
	request.Headers.Set("Authorization", "Bearer "+strings.Repeat("x", 64))
	if p.events(request).StatusCode != 401 {
		t.Fatal("wrong capability accepted")
	}
}

func TestQuotaManualRequestsOnlyJoinSharedSchedule(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	before, _, err := p.observer.Status(p.creds["a"].Account)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/pacer/quota/reconcile", Body: []byte(`{"auth_index":"ia"}`)})
		if r.StatusCode != 200 {
			t.Fatal(string(r.Body))
		}
	}
	after, _, err := p.observer.Status(p.creds["a"].Account)
	if err != nil || !before.Next.Equal(after.Next) || after.Joined != 10 {
		t.Fatal(before, after, err)
	}
	for _, body := range []string{`{"auth_index":"missing"}`, `{"auth_index":"ia","force":true}`, `{"auth_index":"ia"}{}`} {
		if r := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/plugins/pacer/quota/reconcile", Body: []byte(body)}); r.StatusCode < 400 {
			t.Fatal("unsafe refresh accepted", body)
		}
	}
}
