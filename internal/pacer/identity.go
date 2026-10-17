package pacer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
// Recognized header fallbacks cover old CPA builds. Without a usable value the
// chat is recognized by its history (historyKey).
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

// keys returns the request's chat key and its history's chat key. A request
// that names no chat goes by its history.
func (t attempt) keys(provider, model string) (key, history string) {
	key = chatKey(t.Caller, provider, model, t.Session, t.Claim)
	history = historyChatKey(t.Caller, provider, model, t.History)
	if key == "" {
		key = history
	}
	return key, history
}

// historyChatKey is the chat key of a history start (historyKey), within one CPA
// API key, provider and model, like a session key.
func historyChatKey(caller, provider, model, history string) string {
	if caller == "" || history == "" {
		return ""
	}
	return tracker.Digest(caller + "\x00" + provider + "\x00" + model + "\x00history:" + history)
}

// historyKey identifies a chat by how its history starts: the request format,
// tools, system prompt and first user message. Every later request of the chat
// repeats that start, as it is what the provider's prompt cache reads, so a
// request that names no chat still finds it, and a chat behind a new name (a
// fork, a client restart) finds the account holding its cache. Cache markers
// move between requests and are left out. A request without a first user
// message, or one continuing a stored response, has no history to compare: "".
func historyKey(format string, body []byte) string {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	var parts []json.RawMessage
	switch format {
	case "claude":
		_, first := leadingMessages(req["messages"], "role")
		parts = []json.RawMessage{req["tools"], req["system"], first}
	case "openai":
		lead, first := leadingMessages(req["messages"], "role")
		parts = []json.RawMessage{req["tools"], lead, first}
	case "openai-response", "codex":
		if id := strings.TrimSpace(string(req["previous_response_id"])); id != "" && id != "null" && id != `""` {
			return ""
		}
		var text string
		if json.Unmarshal(req["input"], &text) == nil {
			first, _ := json.Marshal(map[string]string{"role": "user", "content": text})
			parts = []json.RawMessage{req["tools"], req["instructions"], nil, first}
			break
		}
		lead, first := leadingMessages(req["input"], "role")
		parts = []json.RawMessage{req["tools"], req["instructions"], lead, first}
	case "gemini":
		system := req["systemInstruction"]
		if len(system) == 0 {
			system = req["system_instruction"]
		}
		_, first := leadingMessages(req["contents"], "role")
		parts = []json.RawMessage{req["tools"], system, first}
	default:
		return ""
	}
	if len(parts[len(parts)-1]) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(format))
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write(canonicalJSON(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// leadingMessages splits a message list at its first user message: the
// system or developer messages before it, as one JSON array, and that message.
// Only the leading entries are decoded, not the whole history.
func leadingMessages(raw json.RawMessage, roleField string) (lead, first json.RawMessage) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, nil
	}
	var before []json.RawMessage
	for _, item := range items {
		var m map[string]json.RawMessage
		if json.Unmarshal(item, &m) != nil {
			return nil, nil
		}
		var role string
		_ = json.Unmarshal(m[roleField], &role)
		if role == "user" {
			if len(before) > 0 {
				lead, _ = json.Marshal(before)
			}
			return lead, item
		}
		before = append(before, item)
	}
	return nil, nil
}

// canonicalJSON re-encodes a value with sorted keys and without cache_control.
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(dropCacheControl(v))
	if err != nil {
		return raw
	}
	return out
}

func dropCacheControl(v any) any {
	switch value := v.(type) {
	case map[string]any:
		delete(value, "cache_control")
		for key, item := range value {
			value[key] = dropCacheControl(item)
		}
	case []any:
		for i, item := range value {
			value[i] = dropCacheControl(item)
		}
	}
	return v
}
