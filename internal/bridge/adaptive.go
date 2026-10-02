package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/adaptive"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/quota"
)

const reservationHeader = "X-Quota-Reservation"

// A private, already mounted file permits hot loading without restarting CPA.
func backendToken(c adaptive.Config) (string, error) {
	if c.TokenFile != "" {
		if c.TokenEnv != "" || !filepath.IsAbs(c.TokenFile) {
			return "", fmt.Errorf("configure one backend token source with an absolute file path")
		}
		f, err := os.Open(c.TokenFile)
		if err != nil {
			return "", fmt.Errorf("backend token file unavailable")
		}
		defer f.Close()
		s, err := f.Stat()
		if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() > 4096 {
			return "", fmt.Errorf("backend token file must be private and regular")
		}
		b, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil || len(b) > 4096 {
			return "", fmt.Errorf("backend token file unreadable")
		}
		token := strings.TrimSpace(string(b))
		if len(token) < 32 || strings.ContainsAny(token, "\r\n") {
			return "", fmt.Errorf("invalid backend token")
		}
		return token, nil
	}
	token := os.Getenv(c.TokenEnv)
	if len(token) < 32 {
		return "", fmt.Errorf("backend token environment variable missing or too short")
	}
	return token, nil
}

func validateBackend(c adaptive.Config) error {
	if !c.Enabled() || c.URL == "" {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid CPAMP backend URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()))) {
			return fmt.Errorf("CPAMP backend requires HTTPS or a private/loopback address")
		}
	}
	_, err = backendToken(c)
	return err
}
func (p *Plugin) exchange() {
	if p.stopped.Load() || p.Engine == nil || !p.exchangeMu.TryLock() {
		return
	}
	defer p.exchangeMu.Unlock()
	if p.stopped.Load() {
		return
	}
	cfg := p.Engine.AdaptiveConfig()
	if !cfg.Enabled() || cfg.URL == "" || time.Now().Before(p.nextExchange) {
		return
	}
	p.nextExchange = time.Now().Add(5 * time.Second)
	if validateBackend(cfg) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"protocol": 1, "readings": p.Engine.Readings(), "projections": p.Engine.Projections()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.URL, "/")+"/internal/quota-handoff/exchange", bytes.NewReader(body))
	if err != nil {
		return
	}
	token, err := backendToken(cfg)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return
	}
	var x adaptive.Exchange
	if json.Unmarshal(raw, &x) != nil {
		return
	}
	_ = p.Engine.ImportModels(x)
}
func (p *Plugin) adaptiveRequest(req pluginapi.SchedulerPickRequest) (adaptive.Request, string) {
	id := http.Header(req.Options.Headers).Get(reservationHeader)
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.requests[id]
	if !ok {
		return adaptive.Request{}, ""
	}
	r := t.Estimate
	r.ID = id
	r.Model = req.Model
	return r, id
}
func (p *Plugin) selected(id string, f handoff.Flight) {
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.requests[id]
	if !ok {
		return
	}
	t.Selected = f
	p.requests[id] = t
}
func (p *Plugin) coverage(id string, windows []quota.Window) []quota.Window {
	started := time.Now().Unix()
	p.mu.Lock()
	if t, ok := p.requests[id]; ok {
		started = t.Started
	}
	p.mu.Unlock()
	for i := range windows {
		windows[i].Covered = started - p.Engine.AdaptiveConfig().LagSeconds
	}
	return windows
}
func (p *Plugin) accountUsage(req pluginapi.UsageRecord) error {
	input := req.Detail.InputTokens
	read := req.Detail.CacheReadTokens
	if read == 0 && req.Detail.CacheCreationTokens == 0 {
		read = req.Detail.CachedTokens
	}
	write := req.Detail.CacheCreationTokens
	// CPA executors publish Claude's uncached input separately; Codex's input
	// includes cached input. Match CPAMP's normalized total-input convention.
	if req.Provider == "claude" && req.ExecutorType != "OpenAICompatExecutor" {
		input += read + write
	}
	model := req.ResponseModel
	if model == "" {
		model = req.Model
	}
	tier := req.ResponseServiceTier
	if tier == "" {
		tier = req.ServiceTier
	}
	return p.Engine.Usage(req.RequestID, req.AuthID, model, tier, float64(input), float64(read), float64(write), float64(req.Detail.OutputTokens), handoff.UsageContext{TraceID: req.TraceID, RequestModel: req.Model, Failed: req.Failed})
}
