package bridge

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Only fixed provider quota URLs are used. The host owns OAuth persistence and
// refresh; credentials live only for this callback and never enter plugin state.
func (p *Plugin) refresh(raw []byte) {
	if p.Host == nil || p.stopped.Load() || !p.refreshMu.TryLock() {
		return
	}
	defer p.refreshMu.Unlock()
	var callback struct {
		ID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &callback)
	for _, c := range p.Engine.Credentials() {
		if p.stopped.Load() {
			return
		}
		runtimeRaw, err := p.Host("host.auth.get_runtime", map[string]string{"auth_index": c.Index})
		if err != nil {
			continue
		}
		var runtime pluginapi.HostAuthGetRuntimeResponse
		if json.Unmarshal(runtimeRaw, &runtime) != nil || runtime.Auth.Disabled || runtime.Auth.ID != c.ID {
			continue
		}
		rawAuth, err := p.Host("host.auth.get", map[string]string{"auth_index": c.Index})
		if err != nil {
			continue
		}
		var auth struct {
			JSON struct {
				AccessToken string `json:"access_token"`
				AccountID   string `json:"account_id"`
				AccountUUID string `json:"account_uuid"`
				Email       string `json:"email"`
			} `json:"json"`
		}
		if json.Unmarshal(rawAuth, &auth) != nil || auth.JSON.AccessToken == "" {
			continue
		}
		identity := auth.JSON.AccountID
		if c.Provider == "claude" {
			identity = auth.JSON.AccountUUID
		}
		if identity == "" {
			identity = strings.ToLower(strings.TrimSpace(auth.JSON.Email))
		}
		if identity != "" {
			_ = p.Engine.Identify(c.ID, identity)
		}
		current, _ := p.Engine.Credential(c.ID)
		key := current.Provider + ":" + current.Account
		if time.Now().Before(p.next[key]) {
			continue
		}
		p.next[key] = time.Now().Add(30 * time.Second)

		url := "https://api.anthropic.com/api/oauth/usage"
		headers := http.Header{"Authorization": {"Bearer " + auth.JSON.AccessToken}, "Accept": {"application/json"}}
		if c.Provider == "claude" {
			headers.Set("anthropic-beta", "oauth-2025-04-20")
		} else {
			url = "https://chatgpt.com/backend-api/wham/usage"
			if auth.JSON.AccountID != "" {
				headers.Set("ChatGPT-Account-Id", auth.JSON.AccountID)
			}
		}
		// Opening an operation allows a hung quota API call to be cancelled without
		// touching an admitted inference stream.
		opRaw, err := p.Host("host.http.operation_open", map[string]string{"host_callback_id": callback.ID})
		if err != nil {
			continue
		}
		var op struct {
			ID string `json:"operation_id"`
		}
		if json.Unmarshal(opRaw, &op) != nil || op.ID == "" {
			continue
		}
		cancelled := make(chan struct{})
		timer := time.AfterFunc(5*time.Second, func() {
			_, _ = p.Host("host.http.cancel", map[string]string{"operation_id": op.ID, "host_callback_id": callback.ID})
			close(cancelled)
		})
		result, err := p.Host("host.http.do", map[string]any{"host_callback_id": callback.ID, "operation_id": op.ID, "method": "GET", "url": url, "headers": headers})
		if !timer.Stop() {
			<-cancelled
		}
		if err != nil {
			continue
		}
		var response pluginapi.HTTPResponse
		if json.Unmarshal(result, &response) != nil {
			continue
		}
		if response.StatusCode == 429 {
			seconds, _ := strconv.Atoi(response.Headers.Get("Retry-After"))
			if seconds < 30 {
				seconds = 30
			}
			if seconds > 3600 {
				seconds = 3600
			}
			p.next[key] = time.Now().Add(time.Duration(seconds) * time.Second)
			continue
		}
		if response.StatusCode != 200 {
			continue
		}
		windows, err := quota.API(c.Provider, response.Body, time.Now().Unix())
		if err != nil {
			continue
		}
		for i := range windows {
			windows[i].Covered = time.Now().Unix() - 5 - p.Engine.AdaptiveConfig().LagSeconds
		}
		for _, w := range windows {
			warning, _ := w.Thresholds()
			if w.Remaining <= warning {
				p.next[key] = time.Now().Add(10 * time.Second)
			}
		}
		_ = p.Engine.Observe(c.ID, windows)
		if p.Engine.NearBoundary(c.ID) {
			p.next[key] = time.Now().Add(5 * time.Second)
		}
	}
}
