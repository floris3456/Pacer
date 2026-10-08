package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

func TestForwardInitNativeOnlyExplicitAndExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forward.sqlite")
	bookPath := filepath.Join(dir, "book.json")
	book := historyreports.ForwardBook{ID: "fixture-v1", Book: historyreports.ForwardComparisonBook{ID: "fixture-v1", Policy: "profile-api-value-v3", Prices: map[string]historyreports.ForwardModelPrice{"claude-opus-5-5": {Prompt: 4, Completion: 20, CacheRead: .2}}}, Qualifications: []historyreports.ForwardPriceQualification{{SourceID: "synthetic-existing-source", Provider: "claude", Model: "claude-opus-5-5", AccountID: "synthetic-account", CredentialRef: "synthetic-credential"}}}
	raw, _ := json.Marshal(book)
	if e := os.WriteFile(bookPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e := forwardInit([]string{"--output", path, "--book-json", bookPath}, &out); e != nil {
		t.Fatal(e)
	}
	var result struct {
		Book string `json:"forward_report_book"`
		Mode string `json:"mode"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Mode != "native-only" || len(result.Book) != 64 {
		t.Fatal(out.String())
	}
	s, e := historyreports.OpenPreparedForward(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.NativeForwardSelection(context.Background(), result.Book); e != nil {
		t.Fatal(e)
	}
	s.Close()
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = forwardInit([]string{"--output", path, "--book-json", bookPath}, &out); e == nil {
		t.Fatal("overwrote prepared store")
	}
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("existing output changed", e)
	}
	for _, bad := range []string{`{"access_token":"SECRET-SENTINEL"}`, `{"id":"a","id":"b"}`, `{"ID":"alias"}`, `{"book":null}`, `{"id":null}`, `{"book":{"prices":{"m":{"unexpected":1}}}}`} {
		if e = os.WriteFile(bookPath, []byte(bad), 0600); e != nil {
			t.Fatal(e)
		}
		e = forwardInit([]string{"--output", filepath.Join(dir, "rejected.sqlite"), "--book-json", bookPath}, &out)
		if e == nil || strings.Contains(e.Error(), "SECRET-SENTINEL") {
			t.Fatal(e)
		}
	}
	if _, e = os.Stat(filepath.Join(dir, "rejected.sqlite")); !os.IsNotExist(e) {
		t.Fatal("invalid input created output")
	}
}
