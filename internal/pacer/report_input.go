package pacer

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
)

// Saved pins are authoritative input, not best-effort options. Unlike ordinary
// encoding/json decoding, this refuses duplicates, case aliases and scalar nulls.
func decodeReport(raw []byte, op string, out *reportRequest) error {
	if err := decodeStrictReport(raw, out); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	allowed := map[string]bool{"query": true}
	switch op {
	case "windows":
		for _, key := range []string{"kind", "state", "limit", "cursor"} {
			allowed[key] = true
		}
	case "evidence":
		allowed["window_id"] = true
	case "export":
		for _, key := range []string{"window_id", "kind", "format", "csv", "profile_references"} {
			allowed[key] = true
		}
	}
	for key := range fields {
		if !allowed[key] {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// Each legacy operation supplies its own request type so even an empty field
// belonging to a different operation is rejected by the same strict scanner.
func decodeStrictReport(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := reportInputValue(d, reflect.TypeOf(out).Elem(), 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return io.ErrUnexpectedEOF
	}
	return json.Unmarshal(raw, out)
}

func reportInputFields(t reflect.Type, fields map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			reportInputFields(f.Type, fields)
			continue
		}
		if name != "" && name != "-" {
			fields[name] = f.Type
		}
	}
}

func reportInputValue(d *json.Decoder, t reflect.Type, depth int) error {
	if depth > 12 {
		return io.ErrUnexpectedEOF
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		// Go-produced optional slices can be null; null pointers/objects and
		// scalar nulls would erase explicit filters, so never accept those.
		if t.Kind() == reflect.Slice {
			return nil
		}
		return io.ErrUnexpectedEOF
	}
	switch t.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return io.ErrUnexpectedEOF
		}
		fields := map[string]reflect.Type{}
		reportInputFields(t, fields)
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			name, ok := k.(string)
			child := fields[name]
			if !ok || child == nil || seen[name] {
				return io.ErrUnexpectedEOF
			}
			seen[name] = true
			if e = reportInputValue(d, child, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return io.ErrUnexpectedEOF
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return io.ErrUnexpectedEOF
		}
		for d.More() {
			if e := reportInputValue(d, t.Elem(), depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return io.ErrUnexpectedEOF
		}
	default:
		if _, ok := token.(json.Delim); ok {
			return io.ErrUnexpectedEOF
		}
		// json.Unmarshal below enforces concrete scalar kinds and ranges.
	}
	return nil
}
