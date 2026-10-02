package notice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var warning = []Warning{{"small/hour/1/warning", "[CPA quota warning] Credential Small has 20.0% of its five-hour quota remaining; resets 2026-10-01T18:00:00Z."}}

func chunk(t *testing.T, s *Stream, raw string, warnings []Warning) string {
	t.Helper()
	out, drop := s.Chunk([]byte(raw), warnings)
	if drop {
		t.Fatal("quota warning dropped a provider chunk")
	}
	if len(out) == 0 {
		return raw
	}
	return string(out)
}
func frame(v map[string]any) string {
	raw, _ := json.Marshal(v)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", v["type"], raw)
}

func TestChatWarningsPreserveToolCallsAndDeduplicatePerThreshold(t *testing.T) {
	s := New("openai", []byte(`{"stream":true}`))
	tool := `{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"x\":1}"}}]}}]}`
	if got := chunk(t, s, tool, warning); got != tool {
		t.Fatal(got)
	}
	text := `{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}],"usage":{"completion_tokens":2}}`
	first := chunk(t, s, text, warning)
	v := decode([]byte(first))
	if !strings.Contains(first, "20.0%") || fmt.Sprint(object(v["usage"])["completion_tokens"]) != "2" {
		t.Fatal(first)
	}
	if got := chunk(t, s, text, warning); got != text {
		t.Fatal("repeated warning", got)
	}
	urgent := []Warning{{"small/hour/1/handoff", "[CPA quota warning] Credential Small has 10.0% remaining."}}
	if got := chunk(t, s, text, urgent); !strings.Contains(got, "10.0%") {
		t.Fatal(got)
	}
	if got := chunk(t, s, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, warning); strings.Contains(got, "quota") {
		t.Fatal(got)
	}
}

func TestStructuredAndUnknownProtocolsPassThrough(t *testing.T) {
	for _, request := range []string{`{"response_format":{"type":"json_object"}}`, `{"text":{"format":{"type":"json_schema"}}}`, `{"output_config":{"format":{"type":"json_schema"}}}`, `{"output_format":{"type":"json_schema"}}`, `{"n":2}`, `bad`} {
		s := New("openai", []byte(request))
		raw := `{"choices":[{"index":0,"delta":{"content":"{\"x\":1}"}}]}`
		if got := chunk(t, s, raw, warning); got != raw {
			t.Fatal(request, got)
		}
	}
	if s := New("gemini", []byte(`{}`)); chunk(t, s, "data: {}\n\n", warning) != "data: {}\n\n" {
		t.Fatal("unknown protocol changed")
	}
}

func TestClaudeOnlyTextChanges(t *testing.T) {
	s := New("claude", []byte(`{}`))
	for _, delta := range []map[string]any{{"type": "thinking_delta", "thinking": "reasoning"}, {"type": "input_json_delta", "partial_json": "{\"x\":"}, {"type": "signature_delta", "signature": "opaque"}} {
		raw := frame(map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
		if got := chunk(t, s, raw, warning); got != raw {
			t.Fatal(got)
		}
	}
	raw := frame(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "hello"}})
	got := chunk(t, s, raw, warning)
	_, v := frameData([]byte(got))
	if !strings.HasSuffix(str(object(v["delta"])["text"]), "hello") || !strings.Contains(got, "20.0%") {
		t.Fatal(got)
	}
}

func TestResponsesStreamAndFinalTextAgree(t *testing.T) {
	s := New("openai-response", []byte(`{}`))
	var visible strings.Builder
	for i, text := range []string{"Hello ", "世界"} {
		warnings := []Warning(nil)
		if i == 1 {
			warnings = warning
		}
		raw := frame(map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": text, "sequence_number": 9007199254740993 + i})
		got := chunk(t, s, raw, warnings)
		_, v := frameData([]byte(got))
		visible.WriteString(str(v["delta"]))
		if fmt.Sprint(v["sequence_number"]) != fmt.Sprint(9007199254740993+i) {
			t.Fatal("sequence changed", got)
		}
	}
	part := func() map[string]any {
		return map[string]any{"type": "output_text", "text": "Hello 世界", "annotations": []any{}}
	}
	item := func() map[string]any {
		return map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{part()}}
	}
	for _, event := range []map[string]any{
		{"type": "response.output_text.done", "item_id": "msg_1", "content_index": 0, "text": "Hello 世界"},
		{"type": "response.content_part.done", "item_id": "msg_1", "content_index": 0, "part": part()},
		{"type": "response.output_item.done", "item": item()},
		{"type": "response.completed", "response": map[string]any{"output": []any{item()}}},
	} {
		got := chunk(t, s, frame(event), warning)
		_, v := frameData([]byte(got))
		var text string
		switch str(v["type"]) {
		case "response.output_text.done":
			text = str(v["text"])
		case "response.content_part.done":
			text = str(object(v["part"])["text"])
		case "response.output_item.done":
			text = str(object(array(object(v["item"])["content"])[0])["text"])
		default:
			text = str(object(array(object(array(object(v["response"])["output"])[0])["content"])[0])["text"])
		}
		if text != visible.String() {
			t.Fatalf("stream and aggregate disagree: %q != %q", text, visible.String())
		}
	}
}

func TestFragmentedMalformedAndOversizedSSEAreNeverDelayed(t *testing.T) {
	s := New("claude", []byte(`{}`))
	raw := frame(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hello"}})
	cut := strings.Index(raw, "hello") + 2
	if got := chunk(t, s, raw[:cut], warning); got != raw[:cut] {
		t.Fatal("first fragment changed")
	}
	if got := chunk(t, s, raw[cut:], warning); got != raw[cut:] {
		t.Fatal("last fragment changed")
	}
	if got := chunk(t, s, raw, warning); !strings.Contains(got, "20.0%") {
		t.Fatal("fragment consumed warning", got)
	}
	for _, bad := range []string{"event: error\ndata: {\"error\":\"upstream\"}\n\n", "data: {broken\n\n", "data: {truncated"} {
		s := New("openai-response", []byte(`{}`))
		if got := chunk(t, s, bad, warning); got != bad {
			t.Fatal(got)
		}
	}
	s = New("claude", []byte(`{}`))
	large := append([]byte("data: {"), bytes.Repeat([]byte("x"), 1<<20)...)
	if got := chunk(t, s, string(large), warning); got != string(large) {
		t.Fatal("oversized payload changed")
	}
}
