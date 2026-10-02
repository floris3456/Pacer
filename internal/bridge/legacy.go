package bridge

import (
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/handoff"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/notice"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type legacyStream struct {
	stream      *notice.Stream
	nextRefresh time.Time
	auth        string
}

func newLegacy(claim *handoff.Claim) *legacyStream {
	if claim != nil {
		return nil
	}
	return &legacyStream{}
}

func (p *Plugin) prepareLegacy(req pluginapi.StreamChunkInterceptRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.requests[req.RequestID]
	if t.Legacy != nil {
		t.Legacy.stream = notice.New(req.SourceFormat, req.OriginalRequest)
		// CPA can omit selected-auth metadata on all response callbacks. The
		// after-auth request hook already captured the actual credential.
		if auth := value(req.Metadata, "selected_auth_id"); auth != "" {
			t.Legacy.auth = auth
		}
	}
}

func (p *Plugin) legacyChunk(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.requests[req.RequestID]
	if t.Legacy == nil || t.Legacy.stream == nil {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	// Poll only while a legacy stream is producing chunks, off the delivery
	// path. refresh applies the existing 30s/10s per-account rate limits.
	if time.Now().After(t.Legacy.nextRefresh) {
		t.Legacy.nextRefresh = time.Now().Add(time.Second)
		go p.refresh(nil)
	}
	events := p.Engine.LegacyWarnings(t.Legacy.auth, req.Model, t.Estimate)
	p.logQuotaEvents(events)
	if p.notificationsMuted.Load() {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	warnings := make([]notice.Warning, 0, len(events))
	for _, event := range events {
		warnings = append(warnings, notice.Warning{Key: event.ID, Text: event.Text})
	}
	body, drop := t.Legacy.stream.Chunk(req.Body, warnings)
	return pluginapi.StreamChunkInterceptResponse{Body: body, DropChunk: drop}
}
