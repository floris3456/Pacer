package pacer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/origin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCodexOriginPoolIntersectsGrantAndLeavesPlaintextUnpinned(t *testing.T) {
	s, err := origin.Open(filepath.Join(t.TempDir(), "state.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p := New(nil)
	p.codexOrigins = s
	p.cfg.ClientAccess = &ClientAccess{Grants: []ClientGrant{{CallerScope: "caller", AuthIDs: []string{"a", "b"}}}}
	if err = s.Observe(context.Background(), origin.Binding{Caller: "caller", AuthID: "a", Identity: "actual-a"}, []byte(`{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"known"}]}`)); err != nil {
		t.Fatal(err)
	}
	req := pluginapi.RequestInterceptRequest{SourceFormat: "openai-response", Body: []byte(`{"input":[{"type":"compaction","encrypted_content":"known"}]}`), Metadata: map[string]any{"caller_scope": "caller"}}
	pool, refusal := p.beforeOrigin(req, &pluginapi.AuthPool{Version: 1, AuthIDs: []string{"a", "b"}})
	if refusal.Terminate || pool == nil || len(pool.AuthIDs) != 1 || pool.AuthIDs[0] != "a" {
		t.Fatalf("pool=%+v refusal=%+v", pool, refusal)
	}
	if _, refusal = p.beforeOrigin(req, &pluginapi.AuthPool{Version: 1, AuthIDs: []string{"b"}}); !refusal.Terminate || refusal.StatusCode != 403 {
		t.Fatalf("unauthorized owner accepted %+v", refusal)
	}
	req.Body = []byte(`{"input":[{"type":"message","role":"user","content":"plaintext"}]}`)
	original := &pluginapi.AuthPool{Version: 1, AuthIDs: []string{"a", "b"}}
	if pool, refusal = p.beforeOrigin(req, original); refusal.Terminate || pool != original {
		t.Fatal("plaintext pinned")
	}
}

func TestCodexOriginUnrelatedFormatsAndLargeClaudeUnaffected(t *testing.T) {
	p := New(nil)
	for _, source := range []string{"claude", "openai", "gemini", "openai-image"} {
		req := pluginapi.RequestInterceptRequest{SourceFormat: source, Body: []byte(strings.Repeat("x", origin.MaxBodyBytes+1))}
		if _, refusal := p.beforeOrigin(req, nil); refusal.Terminate {
			t.Fatalf("unrelated %s body rejected", source)
		}
		if refusal := p.afterOrigin(req); refusal.Terminate {
			t.Fatalf("unrelated %s body rejected after auth", source)
		}
	}
	req := pluginapi.RequestInterceptRequest{SourceFormat: "openai-response", ToFormat: "claude", Body: []byte(`{"input":[{"type":"compaction","encrypted_content":"unknown"}]}`)}
	if _, refusal := p.beforeOrigin(req, nil); !refusal.Terminate {
		t.Fatal("unknown origin allowed without store")
	}
	if refusal := p.afterOrigin(req); !refusal.Terminate {
		t.Fatal("Responses alias crossed provider with opaque state")
	}
}
