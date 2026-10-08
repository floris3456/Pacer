package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

// forward-init creates a new native-only ledger from explicit trusted local
// metadata. It does not open source state, import history or invent source IDs.
func forwardInit(args []string, out io.Writer) error {
	f := flag.NewFlagSet("forward-init", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	path := f.String("output", "", "new absolute forward database path")
	bookPath := f.String("book-json", "", "bounded immutable qualified comparison book JSON")
	if f.Parse(args) != nil || f.NArg() != 0 || *path == "" || *bookPath == "" || out == nil {
		return errors.New("forward-init requires --output PATH and --book-json PATH")
	}
	if historyreports.CheckForwardPath(*path, *bookPath) != nil || historyreports.CheckForwardPath(*bookPath, *path) != nil {
		return errors.New("forward-init paths must be canonical distinct regular files without symlinks")
	}
	file, err := os.Open(*bookPath)
	if err != nil {
		return errors.New("forward-init book file unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	file.Close()
	if err != nil || len(raw) > 2<<20 {
		return errors.New("forward-init book exceeds bounded input")
	}
	var book historyreports.ForwardBook
	if strictForwardJSON(raw, &book) != nil || book.ID == "" || book.Book.ID == "" || book.Book.Policy != "profile-api-value-v3" || book.Book.Historical != nil || len(book.Book.Prices) == 0 {
		return errors.New("forward-init requires a native-only immutable qualified v3 book")
	}
	// Reserve exclusively. The initializer can only adopt this newly owned file;
	// an existing database, hard link or partial prior preparation is never reset.
	reserved, err := os.OpenFile(*path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("forward-init output must not exist and its directory must exist")
	}
	if err = reserved.Close(); err != nil {
		return errors.New("forward-init output reservation failed")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	s, err := historyreports.OpenForward(ctx, *path)
	if err != nil {
		return errors.New("forward-init preparation failed; reserved output retained")
	}
	defer s.Close()
	digest, err := s.RegisterForwardBook(ctx, book)
	if err != nil {
		return errors.New("forward-init book registration failed; reserved output retained")
	}
	if _, err = s.NativeForwardSelection(ctx, digest); err != nil {
		return errors.New("forward-init reference verification failed")
	}
	result := struct {
		Path     string `json:"forward_report_path"`
		Book     string `json:"forward_report_book"`
		Mode     string `json:"mode"`
		Profile  string `json:"profile_revision"`
		Capacity string `json:"capacity"`
	}{*path, digest, "native-only", "0", "unavailable"}
	if err = json.NewEncoder(out).Encode(result); err != nil {
		return errors.New("forward-init result write failed; prepared output retained")
	}
	return nil
}

// Exact field spelling, duplicate rejection and bounded nesting precede normal
// typed decoding. Model-name maps are intentional; arbitrary object fields are
// not. Diagnostics never include caller JSON, paths or a decoder error body.
func strictForwardJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := forwardJSONValue(d, reflect.TypeOf(out).Elem(), 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return io.ErrUnexpectedEOF
	}
	return json.Unmarshal(raw, out)
}
func forwardJSONFields(t reflect.Type, fields map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			forwardJSONFields(f.Type, fields)
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
}
func forwardJSONValue(d *json.Decoder, t reflect.Type, depth int) error {
	if depth > 12 {
		return io.ErrUnexpectedEOF
	}
	nullable := t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if nullable {
			return nil
		}
		return io.ErrUnexpectedEOF
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if token != json.Delim('{') {
			return io.ErrUnexpectedEOF
		}
		fields := map[string]reflect.Type{}
		if t.Kind() == reflect.Struct {
			forwardJSONFields(t, fields)
		}
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			name, ok := key.(string)
			if e != nil || !ok || seen[name] || len(seen) >= 10000 {
				return io.ErrUnexpectedEOF
			}
			seen[name] = true
			child := fields[name]
			if t.Kind() == reflect.Map {
				child = t.Elem()
			}
			if child == nil {
				return io.ErrUnexpectedEOF
			}
			if e = forwardJSONValue(d, child, depth+1); e != nil {
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
		n := 0
		for d.More() {
			n++
			if n > 10000 {
				return io.ErrUnexpectedEOF
			}
			if e := forwardJSONValue(d, t.Elem(), depth+1); e != nil {
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
	}
	return nil
}
