package pacer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageWatchFansOutOtherCallersHeadersBeforeStreamFinishes(t *testing.T) {
	f := newRoutingFixture(t, "off", false)
	p := f.p
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer viewer-one" && r.Header.Get("Authorization") != "Bearer viewer-two" {
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
	results := make(chan pluginapi.ManagementResponse, 2)
	for _, key := range []string{"viewer-one", "viewer-two"} {
		go func(key string) { results <- p.events(request(key, first.Cursor)) }(key)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.usageChanges.mu.Lock()
		waiting := p.usageChanges.waiters
		p.usageChanges.mu.Unlock()
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("viewers never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
	f.clock.Add(int64(time.Second))
	// A third user, without X-Pacer-Client or a session claim, begins a response.
	// Neither viewer sends an inference request, and the stream is still open.
	metadata := map[string]any{"caller_scope": "third-user", "selected_auth_id": "a"}
	req := pluginapi.RequestInterceptRequest{RequestID: "other-users-stream", Model: "claude-opus-5-5", Metadata: metadata}
	invoke(t, p, pluginabi.MethodRequestInterceptBefore, req)
	invoke(t, p, pluginabi.MethodRequestInterceptAfter, req)
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.69"}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(p.now().Add(3*time.Hour).Unix(), 10)}}
	invoke(t, p, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, Metadata: metadata, ChunkIndex: -1, ResponseHeaders: headers})
	for i := 0; i < 2; i++ {
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
	close(done)
	if _, ok := u.wait(next, 20*time.Second, done); ok {
		t.Fatal("shutdown left wait alive")
	}
	var restarted usageChanges
	if got, ok := restarted.wait(next, 20*time.Second, make(chan struct{})); !ok || got == next {
		t.Fatal("restart reused cursor")
	}
}
