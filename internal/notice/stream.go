// Package notice adds labelled quota text to uncoordinated HTTP text streams.
// It never adds errors, tool arguments, reasoning, or compaction instructions.
package notice

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"strings"
)

type Warning struct{ Key, Text string }
type insertion struct {
	offset int
	text   string
}
type textState struct {
	bytes   int
	digest  hash.Hash
	inserts []insertion
}
type Stream struct {
	format  string
	enabled bool
	pending []byte
	seen    map[string]bool
	texts   map[string]*textState
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func str(v any) string            { s, _ := v.(string); return s }
func decode(raw []byte) map[string]any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var v map[string]any
	if !json.Valid(raw) || decoder.Decode(&v) != nil {
		return nil
	}
	return v
}

func New(format string, request []byte) *Stream {
	s := &Stream{format: format, seen: map[string]bool{}, texts: map[string]*textState{}}
	r := decode(request)
	if r == nil || (format != "openai" && format != "openai-response" && format != "claude") {
		return s
	}
	// These protocols cannot carry arbitrary prose inside their output contract.
	for _, v := range []any{r["response_format"], object(r["text"])["format"], r["output_format"], object(r["output_config"])["format"]} {
		if v != nil && str(object(v)["type"]) != "text" {
			return s
		}
	}
	if n := r["n"]; n != nil && fmt.Sprint(n) != "1" {
		return s
	}
	s.enabled = true
	return s
}

func (s *Stream) warning(warnings []Warning) string {
	var lines []string
	for _, warning := range warnings {
		if warning.Key == "" || warning.Text == "" || s.seen[warning.Key] || len(s.seen) >= 128 {
			continue
		}
		s.seen[warning.Key] = true
		lines = append(lines, warning.Text)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(lines, "\n") + "\n\n"
}

// Chunk never delays or drops provider bytes. Split SSE frames pass through
// unchanged; a bounded copy lets us track their text for later aggregate events.
// Warnings wait for a complete text frame. CPA owns errors and stream closure.
func (s *Stream) Chunk(body []byte, warnings []Warning) ([]byte, bool) {
	if !s.enabled {
		return nil, false
	}
	if s.format == "openai" {
		v := decode(body)
		if v == nil {
			return nil, false
		}
		choices := array(v["choices"])
		if len(choices) != 1 {
			return nil, false
		}
		choice := object(choices[0])
		if fmt.Sprint(choice["index"]) != "0" {
			return nil, false
		}
		delta := object(choice["delta"])
		text := str(delta["content"])
		if text == "" {
			return nil, false
		}
		warning := s.warning(warnings)
		if warning == "" {
			return nil, false
		}
		delta["content"] = warning + text
		out, _ := json.Marshal(v)
		return out, false
	}
	previous := len(s.pending)
	s.pending = append(s.pending, body...)
	if len(s.pending) > 1<<20 {
		s.enabled = false
		s.pending = nil
		return nil, false
	}
	var out []byte
	for len(s.pending) > 0 {
		length := frameLength(s.pending)
		if length == 0 {
			// Some CPA translators yield one complete event without a blank
			// delimiter. A complete JSON data field is a safe boundary too.
			if _, v := frameData(s.pending); v != nil {
				length = len(s.pending)
			}
		}
		if length == 0 {
			out = append(out, s.pending[previous:]...)
			break
		}
		frame := s.pending[:length]
		if previous > 0 {
			// Earlier bytes were already delivered. Observe the complete frame
			// without rewriting any of it or consuming an undelivered warning.
			_ = s.frame(frame, nil)
			out = append(out, frame[previous:]...)
			previous = 0
		} else {
			out = append(out, s.frame(frame, warnings)...)
		}
		s.pending = s.pending[length:]
	}
	return out, false
}

func frameLength(raw []byte) int {
	length := 0
	for _, separator := range [][]byte{[]byte("\n\n"), []byte("\r\n\r\n")} {
		if index := bytes.Index(raw, separator); index >= 0 && (length == 0 || index+len(separator) < length) {
			length = index + len(separator)
		}
	}
	return length
}
func frameData(raw []byte) ([]string, map[string]any) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var data []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return lines, decode([]byte(strings.Join(data, "\n")))
}
func (s *Stream) frame(raw []byte, warnings []Warning) []byte {
	lines, v := frameData(raw)
	if v == nil {
		return raw
	}
	changed := false
	if s.format == "claude" && str(v["type"]) == "content_block_delta" {
		delta := object(v["delta"])
		if str(delta["type"]) == "text_delta" && str(delta["text"]) != "" {
			if warning := s.warning(warnings); warning != "" {
				delta["text"] = warning + str(delta["text"])
				changed = true
			}
		}
	}
	if s.format == "openai-response" {
		changed = s.response(v, warnings)
	}
	if !changed {
		return raw
	}
	body, _ := json.Marshal(v)
	var out []string
	written := false
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			if !written {
				out = append(out, "data: "+string(body))
				written = true
			}
		} else {
			out = append(out, line)
		}
	}
	return []byte(strings.Join(out, "\n"))
}

func textKey(id any, index any) string {
	if str(id) == "" || index == nil {
		return ""
	}
	return str(id) + "/" + fmt.Sprint(index)
}
func (s *Stream) response(v map[string]any, warnings []Warning) bool {
	key := textKey(v["item_id"], v["content_index"])
	switch str(v["type"]) {
	case "response.output_text.delta":
		text := str(v["delta"])
		if key == "" || text == "" {
			return false
		}
		t := s.texts[key]
		if t == nil {
			if len(s.texts) >= 128 {
				return false
			}
			t = &textState{digest: sha256.New()}
			s.texts[key] = t
		}
		warning := s.warning(warnings)
		if warning != "" {
			t.inserts = append(t.inserts, insertion{t.bytes, warning})
			v["delta"] = warning + text
		}
		t.bytes += len(text)
		_, _ = t.digest.Write([]byte(text))
		return warning != ""
	case "response.output_text.done":
		return s.patch(v, "text", key)
	case "response.content_part.done":
		return s.patch(object(v["part"]), "text", key)
	case "response.output_item.done":
		return s.patchItem(object(v["item"]))
	case "response.completed", "response.incomplete":
		changed := false
		for _, item := range array(object(v["response"])["output"]) {
			changed = s.patchItem(object(item)) || changed
		}
		return changed
	}
	return false
}
func (s *Stream) patchItem(item map[string]any) bool {
	if str(item["type"]) != "message" {
		return false
	}
	changed := false
	for index, raw := range array(item["content"]) {
		part := object(raw)
		if str(part["type"]) == "output_text" {
			changed = s.patch(part, "text", textKey(item["id"], index)) || changed
		}
	}
	return changed
}
func (s *Stream) patch(v map[string]any, field, key string) bool {
	t := s.texts[key]
	if t == nil || len(t.inserts) == 0 {
		return false
	}
	text := str(v[field])
	digest := sha256.Sum256([]byte(text))
	if len(text) != t.bytes || !bytes.Equal(digest[:], t.digest.Sum(nil)) {
		return false
	}
	var out strings.Builder
	last := 0
	for _, add := range t.inserts {
		out.WriteString(text[last:add.offset])
		out.WriteString(add.text)
		last = add.offset
	}
	out.WriteString(text[last:])
	v[field] = out.String()
	return true
}
