package origin

import (
	"bytes"
	"encoding/json"
	"io"
)

const (
	MaxBodyBytes = 4 << 20
	MaxMarkers   = 128
	maxCipher    = 1 << 20
)

type marker struct{ kind, value string }

// Strict bounded JSON rejects ambiguity rather than allowing a second parser's
// duplicate-key interpretation to change the state that admission inspected.
func decodeObject(body []byte) (map[string]any, error) {
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return nil, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	nodes := 0
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		nodes++
		if depth > 64 || nodes > 200000 {
			return nil, ErrInvalid
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalid
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				out := map[string]any{}
				for decoder.More() {
					token, err := decoder.Token()
					name, ok := token.(string)
					if err != nil || !ok {
						return nil, ErrInvalid
					}
					if _, duplicate := out[name]; duplicate {
						return nil, ErrInvalid
					}
					value, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					out[name] = value
				}
				if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
					return nil, ErrInvalid
				}
				return out, nil
			case '[':
				out := []any{}
				for decoder.More() {
					value, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					out = append(out, value)
				}
				if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
					return nil, ErrInvalid
				}
				return out, nil
			default:
				return nil, ErrInvalid
			}
		}
		return token, nil
	}
	value, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}
	return object, nil
}

type markers struct {
	items []marker
	seen  map[marker]bool
}

func (m *markers) add(kind string, value any, maximum int) error {
	s, ok := value.(string)
	if !ok || !safeText(s, maximum) {
		return ErrInvalid
	}
	mark := marker{kind, s}
	if m.seen == nil {
		m.seen = map[marker]bool{}
	}
	if m.seen[mark] {
		return nil
	}
	if len(m.items) >= MaxMarkers {
		return ErrInvalid
	}
	m.seen[mark] = true
	m.items = append(m.items, mark)
	return nil
}

func (m *markers) conversation(value any) error {
	if value == nil {
		return nil
	}
	if object, ok := value.(map[string]any); ok {
		return m.add("conversation", object["id"], 512)
	}
	return m.add("conversation", value, 512)
}

func (m *markers) item(value any, response bool) error {
	item, ok := value.(map[string]any)
	if !ok {
		return ErrInvalid
	}
	if response && item["status"] != nil && item["status"] != "completed" {
		return ErrUntrusted
	}
	typ, _ := item["type"].(string)
	if typ == "item_reference" {
		return m.add("item", item["id"], 512)
	}
	cipher, encrypted := item["encrypted_content"]
	if encrypted && cipher != nil {
		if typ != "compaction" && typ != "reasoning" {
			return ErrInvalid
		}
		if err := m.add("cipher", cipher, maxCipher); err != nil {
			return err
		}
	} else if typ == "compaction" {
		return ErrInvalid
	}
	if id, exists := item["id"]; exists && id != nil && (response || typ == "reasoning" || typ == "compaction") {
		return m.add("item", id, 512)
	}
	return nil
}

func requestMarkers(body []byte) ([]marker, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	var m markers
	if _, exists := object["encrypted_content"]; exists {
		return nil, ErrInvalid
	}
	if previous := object["previous_response_id"]; previous != nil {
		if err := m.add("response", previous, 512); err != nil {
			return nil, err
		}
	}
	if err := m.conversation(object["conversation"]); err != nil {
		return nil, err
	}
	if input := object["input"]; input != nil {
		switch input := input.(type) {
		case string:
		case []any:
			for _, item := range input {
				if err := m.item(item, false); err != nil {
					return nil, err
				}
			}
		default:
			return nil, ErrInvalid
		}
	}
	return m.items, nil
}

func responseMarkers(body []byte) ([]marker, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	if _, event := object["type"]; event {
		if object["type"] != "response.completed" || incomplete(object) {
			return nil, ErrUntrusted
		}
		object, _ = object["response"].(map[string]any)
		if object == nil {
			return nil, ErrInvalid
		}
	}
	if _, nestedEvent := object["type"]; nestedEvent || incomplete(object) {
		return nil, ErrUntrusted
	}
	if kind := object["object"]; kind != nil && kind != "response" && kind != "response.compaction" {
		return nil, ErrUntrusted
	}
	if object["object"] != "response.compaction" && object["status"] != "completed" {
		return nil, ErrUntrusted
	}
	output, ok := object["output"].([]any)
	if !ok {
		return nil, ErrInvalid
	}
	var m markers
	if id := object["id"]; id != nil {
		if err := m.add("response", id, 512); err != nil {
			return nil, err
		}
	}
	if err := m.conversation(object["conversation"]); err != nil {
		return nil, err
	}
	for _, item := range output {
		if err := m.item(item, true); err != nil {
			return nil, err
		}
	}
	return m.items, nil
}

func incomplete(object map[string]any) bool {
	return object["error"] != nil || object["incomplete_details"] != nil || object["status"] != nil && object["status"] != "completed"
}
