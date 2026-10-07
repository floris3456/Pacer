package pacer

import (
	"net/http"
	"testing"
)

func TestClientSessionsReachAffinityWithoutCrossCallerBinding(t *testing.T) {
	for _, tc := range []struct{ header, want string }{{"X-Session-Affinity", "header:chat"}, {"X-Claude-Code-Session-Id", "claude:chat"}, {"Session-Id", "codex:chat"}, {"X-Client-Request-Id", "header:chat"}, {"X-Session-ID", "header:chat"}} {
		sid := session(http.Header{tc.header: {"chat"}}, nil)
		if sid != tc.want {
			t.Fatalf("%s: %s", tc.header, sid)
		}
		if chatKey("a", "claude", "opus", sid, nil) == chatKey("b", "claude", "opus", sid, nil) {
			t.Fatal("cross-caller binding")
		}
	}
	if session(http.Header{"X-Session-Affinity": {"chat"}}, map[string]any{"canonical_session_id": "header:chat:agent:child"}) != "header:chat:agent:child" {
		t.Fatal("host fork identity lost")
	}
	if session(http.Header{"X-Session-Affinity": {"chat\nspoof"}}, nil) != "" {
		t.Fatal("invalid session accepted")
	}
	if _, err := claim(http.Header{controlHeader: {"not-base64!"}}); err == nil {
		t.Fatal("malformed claim admitted")
	}
}
