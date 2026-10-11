package pacer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/observation"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func awaitUsageWaiters(t *testing.T, u *usageChanges, want int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		u.mu.Lock()
		waiting := u.waiters
		u.mu.Unlock()
		if waiting == want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("got %d usage waiters, want %d", waiting, want)
		}
	}
}

func usageWatchRequest(token, model, cursor, wait string) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + token}}, Query: url.Values{"view": {"usage"}, "v": {"1"}, "model": {model}, "cursor": {cursor}, "wait_ms": {wait}}}
}

func awaitUsageResponse(t *testing.T, results <-chan pluginapi.ManagementResponse) pluginapi.ManagementResponse {
	t.Helper()
	select {
	case response := <-results:
		return response
	case <-time.After(3 * time.Second):
		t.Fatal("usage watch did not return")
		return pluginapi.ManagementResponse{}
	}
}

func TestUsageWatchFansOutOtherCallersHeadersBeforeStreamFinishes(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	p := f.p
	var authCalls atomic.Int64
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authCalls.Add(1)
		}
		if r.Header.Get("Authorization") != "Bearer viewer-one" && r.Header.Get("Authorization") != "Bearer viewer-two" && r.Header.Get("Authorization") != "Bearer third-user" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer auth.Close()
	p.mu.Lock()
	p.cfg.UsageAuthURL = auth.URL + "/v1/models"
	p.mu.Unlock()
	request := func(key, cursor string) pluginapi.ManagementRequest {
		return pluginapi.ManagementRequest{Method: "GET", Headers: http.Header{"Authorization": {"Bearer " + key}}, Query: url.Values{"auth": {"api-key"}, "view": {"usage"}, "all": {"true"}, "v": {"1"}, "wait_ms": {"20000"}, "cursor": {cursor}}}
	}
	initial := p.events(request("viewer-one", ""))
	var first struct {
		Cursor string `json:"cursor"`
	}
	if json.Unmarshal(initial.Body, &first) != nil || initial.StatusCode != 200 || first.Cursor == "" {
		t.Fatal(string(initial.Body))
	}
	// Authentication failures never join the held subscription.
	if got := p.events(request("invalid", first.Cursor)); got.StatusCode != 401 {
		t.Fatal(got)
	}
	results := make(chan pluginapi.ManagementResponse, 3)
	for _, key := range []string{"viewer-one", "viewer-two", "third-user"} {
		go func(key string) { results <- p.events(request(key, first.Cursor)) }(key)
	}
	awaitUsageWaiters(t, &p.usageChanges, 3)
	f.clock.Add(int64(time.Second))
	// A third user, without X-Pacer-Client or a session claim, begins a response.
	// Neither viewer sends an inference request, and the stream is still open.
	metadata := map[string]any{"caller_scope": "third-user", "selected_auth_id": "a"}
	req := pluginapi.RequestInterceptRequest{RequestID: "other-users-stream", Model: "claude-opus-5-5", Metadata: metadata}
	invoke(t, p, pluginabi.MethodRequestInterceptBefore, req)
	invoke(t, p, pluginabi.MethodRequestInterceptAfter, req)
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.69"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(p.now().Add(3*time.Hour).Unix(), 10)}}
	invoke(t, p, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, Metadata: metadata, ChunkIndex: -1, ResponseHeaders: headers})
	for i := 0; i < 3; i++ {
		select {
		case result := <-results:
			var view struct {
				Cursor      string `json:"cursor"`
				NowMS       int64  `json:"now_ms"`
				Credentials []struct {
					ID      string        `json:"id"`
					Windows []quotaWindow `json:"windows"`
				} `json:"credentials"`
			}
			if result.StatusCode != 200 || json.Unmarshal(result.Body, &view) != nil || view.Cursor == first.Cursor || view.NowMS != p.now().UnixMilli() {
				t.Fatal(string(result.Body))
			}
			found := false
			for _, c := range view.Credentials {
				for _, w := range c.Windows {
					if c.ID == "a" && w.Seconds == 18000 && w.Left != nil && *w.Left == 31 && w.Source == "header" {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("idle viewer missed other caller's header", string(result.Body))
			}
		case <-time.After(3 * time.Second):
			t.Fatal("header did not immediately wake idle viewer")
		}
	}
	if got := authCalls.Load(); got != 5 {
		t.Fatalf("watch performed %d authentication reads, want one per request", got)
	}
	awaitUsageWaiters(t, &p.usageChanges, 0)
}

func TestUsageWatchReloadsBindingAfterWait(t *testing.T) {
	f := previewFixture(t)
	cursor, _ := f.p.usageChanges.wait("", 0, f.p.workerDone)
	results := make(chan pluginapi.ManagementResponse, 1)
	go func() { results <- f.p.events(usageWatchRequest(previewCapability, f.q.Route, cursor, "20000")) }()
	awaitUsageWaiters(t, &f.p.usageChanges, 1)
	f.clock.Add(int64(time.Second))
	f.q.Fresh = true
	r := f.before(t, "switch-while-waiting", "claude-opus-5-5")
	if after := f.after(t, r, "b"); after.Terminate {
		t.Fatal(string(after.ResponseBody))
	}
	f.p.mu.Lock()
	f.p.requests["another-callers-request"] = attempt{Key: "another-caller", Auth: "a"}
	f.p.mu.Unlock()
	f.p.observeAuth("b", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.5"}}, f.p.now())
	response := awaitUsageResponse(t, results)
	var view struct {
		Cursor   string      `json:"cursor"`
		Current  string      `json:"current"`
		NowMS    int64       `json:"now_ms"`
		LastUsed int64       `json:"last_used"`
		Active   []string    `json:"active"`
		Next     nextUseView `json:"next_use"`
	}
	current := f.binding(t)
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil || view.Current != "b" || view.LastUsed != current.LastUsed.Unix() || view.NowMS != f.p.now().UnixMilli() || view.Cursor == cursor {
		t.Fatal("held watch returned an old binding", string(response.Body))
	}
	if len(view.Active) != 1 || view.Active[0] != "b" || view.Next != f.p.nextUse(current) {
		t.Fatal("held watch mixed chat state", string(response.Body))
	}
	f.complete(t, r.RequestID)
}

func TestUsageWatchCapabilityViewsKeepCallerScopeAndSharedPools(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	f.p.creds["c"] = credential{ID: "c", Account: "codex:watch", Provider: "codex"}
	type caller struct {
		auth, provider, model, token, route string
		all                                 bool
	}
	callers := []caller{
		{auth: "a", provider: "claude", model: "claude-opus-5-5", token: strings.Repeat("a", 64), route: "claude/opus"},
		{auth: "b", provider: "claude", model: "claude-sonnet-4-6", token: strings.Repeat("b", 64), route: "claude/sonnet"},
		{auth: "c", provider: "codex", model: "gpt-5-codex", token: strings.Repeat("c", 64), route: "codex/main", all: true},
	}
	for _, viewer := range callers {
		q := f.q
		q.Route, q.Capability = viewer.route, tracker.Digest(viewer.token)
		key := tracker.Key("caller-"+viewer.auth, viewer.provider, q)
		c := f.p.creds[viewer.auth]
		if _, err := f.p.chats.Admit("active-"+c.ID, key, tracker.Selection{Auth: c.ID, Account: c.Account, Provider: c.Provider, Model: viewer.model}, &q, true); err != nil {
			t.Fatal(err)
		}
		f.p.requests["active-"+c.ID] = attempt{Key: key, Auth: c.ID}
	}
	var hostCalls atomic.Int64
	f.p.Host = func(method string, input any) (json.RawMessage, error) {
		hostCalls.Add(1)
		return testHost(method, input)
	}
	cursor, _ := f.p.usageChanges.wait("", 0, f.p.workerDone)
	results := make(chan pluginapi.ManagementResponse, 3)
	for _, viewer := range callers {
		req := usageWatchRequest(viewer.token, viewer.route, cursor, "20000")
		req.Query.Set("all", strconv.FormatBool(viewer.all))
		go func() { results <- f.p.events(req) }()
	}
	awaitUsageWaiters(t, &f.p.usageChanges, 3)
	f.clock.Add(int64(time.Second))
	f.p.observeAuth("a", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.3"}}, f.p.now())
	for range 3 {
		response := awaitUsageResponse(t, results)
		var view struct {
			Current     string   `json:"current"`
			Provider    string   `json:"provider"`
			Model       string   `json:"model"`
			Active      []string `json:"active"`
			Credentials []struct {
				ID       string        `json:"id"`
				Provider string        `json:"provider"`
				Windows  []quotaWindow `json:"windows"`
			} `json:"credentials"`
		}
		if response.StatusCode != 200 || json.Unmarshal(response.Body, &view) != nil {
			t.Fatal(response.StatusCode, string(response.Body))
		}
		var viewer caller
		for _, candidate := range callers {
			if candidate.auth == view.Current {
				viewer = candidate
			}
		}
		if viewer.auth == "" || view.Provider != viewer.provider || view.Model != viewer.model || len(view.Active) != 1 || view.Active[0] != viewer.auth {
			t.Fatal("watch crossed caller binding scope", string(response.Body))
		}
		wantRows := 2
		if viewer.all {
			wantRows = 3
		}
		foundShared := false
		for _, row := range view.Credentials {
			if !viewer.all && row.Provider != viewer.provider {
				t.Fatal("watch crossed requested provider scope", string(response.Body))
			}
			for _, w := range row.Windows {
				if row.ID == "a" && w.Seconds == 18000 && w.Left != nil && *w.Left == 70 {
					foundShared = true
				}
			}
		}
		if len(view.Credentials) != wantRows || !foundShared {
			t.Fatal("overlapping views missed shared quota", string(response.Body))
		}
	}
	if got := hostCalls.Load(); got != 0 {
		t.Fatalf("capability watch reads made %d host calls", got)
	}
}

func TestUsageWatchRevalidatesCapabilityAfterWait(t *testing.T) {
	for _, change := range []string{"revoked", "different-caller", "different-route-on-timeout"} {
		t.Run(change, func(t *testing.T) {
			f := previewFixture(t)
			cursor, _ := f.p.usageChanges.wait("", 0, f.p.workerDone)
			wait := "20000"
			if change == "different-route-on-timeout" {
				wait = "500"
			}
			results := make(chan pluginapi.ManagementResponse, 1)
			go func() { results <- f.p.events(usageWatchRequest(previewCapability, f.q.Route, cursor, wait)) }()
			awaitUsageWaiters(t, &f.p.usageChanges, 1)
			if _, err := f.p.db.Exec("DELETE FROM pacer_chats WHERE key=?", f.binding(t).Key); err != nil {
				t.Fatal(err)
			}
			if change != "revoked" {
				q := f.q
				if change == "different-route-on-timeout" {
					q.Route = "another/route"
				}
				c := f.p.creds["b"]
				if _, err := f.p.chats.Admit("replacement", tracker.Key("another-caller", c.Provider, q), tracker.Selection{Auth: c.ID, Account: c.Account, Provider: c.Provider, Model: "claude-opus-5-5"}, &q, true); err != nil {
					t.Fatal(err)
				}
			}
			if change != "different-route-on-timeout" {
				f.p.usageChanges.notify()
			}
			response := awaitUsageResponse(t, results)
			if response.StatusCode != 401 || strings.Contains(string(response.Body), "credentials") || strings.Contains(string(response.Body), "cursor") {
				t.Fatal("old held capability retained access", response.StatusCode, string(response.Body))
			}
			awaitUsageWaiters(t, &f.p.usageChanges, 0)
		})
	}
}

func TestUsageWatchNotifiesOnlyCanonicalChanges(t *testing.T) {
	for _, provider := range []string{"claude", "antigravity"} {
		t.Run(provider, func(t *testing.T) {
			f := newRoutingFixture(t, "off", false)
			c := f.p.creds["a"]
			c.Provider, c.Account = provider, provider+":watch-account"
			left := 80.0
			at := f.p.now()
			sample := observation.Window{Provider: provider, Scope: "all", Seconds: 18000, Remaining: &left, Reset: at.Add(time.Hour), Usage: observation.Field{At: at, Source: "header"}, ResetEvidence: observation.Field{At: at, Source: "header", Quality: "absolute"}}
			f.p.storeObservations(c, []observation.Window{sample})
			for _, change := range []string{"evidence-time", "balance", "conflict", "dormant"} {
				cursor, _ := f.p.usageChanges.wait("", 0, f.p.workerDone)
				results := make(chan string, 3)
				for range 3 {
					go func() {
						next, _ := f.p.usageChanges.wait(cursor, 20*time.Second, f.p.workerDone)
						results <- next
					}()
				}
				awaitUsageWaiters(t, &f.p.usageChanges, 3)
				// Replayed and out-of-order source evidence stays in audit history,
				// but cannot wake every viewer when the canonical window is unchanged.
				f.p.storeObservations(c, []observation.Window{sample})
				f.p.storeObservations(c, []observation.Window{sample})
				stale := sample
				stale.Usage.At, stale.ResetEvidence.At = at.Add(-time.Second), at.Add(-time.Second)
				f.p.storeObservations(c, []observation.Window{stale})
				if got, _ := f.p.usageChanges.wait("", 0, f.p.workerDone); got != cursor {
					t.Fatalf("%s: duplicate or stale observation woke viewers", change)
				}
				switch change {
				case "evidence-time":
					f.clock.Add(int64(time.Second))
					sample.Usage.At = f.p.now()
				case "balance":
					f.clock.Add(int64(time.Second))
					left = 70
					sample.Usage.At = f.p.now()
				case "conflict":
					left = 60 // Equal-time contradiction changes the canonical status.
				case "dormant":
					f.clock.Add(int64(time.Second))
					sample = observation.Window{Provider: provider, Scope: "all", Seconds: 18000, Dormant: true, StatusEvidence: observation.Field{At: f.p.now(), Source: "api"}}
				}
				f.p.storeObservations(c, []observation.Window{sample})
				next, _ := f.p.usageChanges.wait("", 0, f.p.workerDone)
				if next == cursor {
					t.Fatalf("%s: material observation did not advance cursor", change)
				}
				for range 3 {
					select {
					case got := <-results:
						if got != next {
							t.Fatalf("%s: viewer missed canonical revision: %s != %s", change, got, next)
						}
					case <-time.After(3 * time.Second):
						t.Fatalf("%s: canonical change did not wake viewer", change)
					}
				}
				awaitUsageWaiters(t, &f.p.usageChanges, 0)
			}
		})
	}
}

func TestUsageWatchSaturationReturns503AndReleasesWaiters(t *testing.T) {
	f := previewFixture(t)
	cursor, _ := f.p.usageChanges.wait("", 0, f.p.workerDone)
	results := make(chan bool, 256)
	for range 256 {
		go func() {
			_, ok := f.p.usageChanges.wait(cursor, 20*time.Second, f.p.workerDone)
			results <- ok
		}()
	}
	awaitUsageWaiters(t, &f.p.usageChanges, 256)
	response := f.p.events(usageWatchRequest(previewCapability, f.q.Route, cursor, "20000"))
	if response.StatusCode != 503 || !strings.Contains(string(response.Body), "usage_watch_unavailable") || strings.Contains(string(response.Body), "cursor") {
		t.Fatal("saturation must remain a retryable failure", response.StatusCode, string(response.Body))
	}
	f.p.Close()
	for range 256 {
		select {
		case ok := <-results:
			if ok {
				t.Fatal("shutdown returned a successful snapshot")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown left a waiter alive")
		}
	}
	awaitUsageWaiters(t, &f.p.usageChanges, 0)
}

func TestUsageWatchCursorRacesTimeoutAndShutdown(t *testing.T) {
	var u usageChanges
	done := make(chan struct{})
	first, _ := u.wait("", 0, done)
	u.notify() // A change between GETs cannot be lost.
	next, ok := u.wait(first, 20*time.Second, done)
	if !ok || next == first {
		t.Fatal("lost intervening change")
	}
	if got, ok := u.wait(next, time.Millisecond, done); !ok || got != next {
		t.Fatal("heartbeat changed cursor")
	}
	awaitUsageWaiters(t, &u, 0)
	close(done)
	if _, ok := u.wait(next, 20*time.Second, done); ok {
		t.Fatal("shutdown left wait alive")
	}
	if _, ok := u.wait("", 0, done); ok {
		t.Fatal("shutdown accepted a new snapshot")
	}
	awaitUsageWaiters(t, &u, 0)
	var restarted usageChanges
	if got, ok := restarted.wait(next, 20*time.Second, make(chan struct{})); !ok || got == next {
		t.Fatal("restart reused cursor")
	}
}
