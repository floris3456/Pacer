package pacer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/tracker"
)

const controlHeader = "X-Quota-Handoff"
const requestHeader = "X-Pacer-Request"

func claim(headers http.Header) (*tracker.Claim, error) {
	raw := header(headers, controlHeader)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 4096 {
		return nil, errors.New("quota claim too large")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("invalid quota claim encoding")
	}
	var c tracker.Claim
	if json.Unmarshal(decoded, &c) != nil || !c.Valid() {
		return nil, errors.New("invalid quota claim")
	}
	return &c, nil
}

func header(headers http.Header, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func text(metadata map[string]any, key string) string { v, _ := metadata[key].(string); return v }

// Prefer the host's canonical identity, which includes subagent/fork semantics.
// Recognized header fallbacks cover old CPA builds. An ambiguous explicit value
// causes routing fallback rather than a speculative body-prefix identity.
func session(headers http.Header, metadata map[string]any) string {
	if id := text(metadata, "canonical_session_id"); validID(id) {
		return id
	}
	for _, item := range []struct{ key, prefix string }{{"X-Claude-Code-Session-Id", "claude:"}, {"Session-Id", "codex:"}, {"Session_id", "codex:"}, {"X-Session-ID", "header:"}, {"X-Session-Affinity", "header:"}, {"X-Client-Request-Id", "header:"}} {
		if id := header(headers, item.key); id != "" {
			if validID(id) {
				return item.prefix + id
			}
			return ""
		}
	}
	return ""
}

func validID(id string) bool {
	return id != "" && len(id) <= 512 && !strings.ContainsAny(id, "\r\n\x00") && strings.TrimSpace(id) == id
}

func chatKey(caller, provider, model, sid string, q *tracker.Claim) string {
	if caller == "" {
		return ""
	}
	if q != nil {
		return tracker.Key(caller, provider, *q)
	}
	if sid == "" {
		return ""
	}
	return tracker.Digest(caller + "\x00" + provider + "\x00" + model + "\x00" + sid)
}
