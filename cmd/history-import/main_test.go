package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
)

func TestCLIRejectsUnattestedAndSecretArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--unknown=sk-secret"}, {"--source", "sk-secret", "--store", "unused", "--manifest", "unused"}} {
		var out bytes.Buffer
		e := run(context.Background(), args, &out)
		if !errors.Is(e, history.ErrInput) || out.Len() != 0 || strings.Contains(e.Error(), "sk-secret") {
			t.Fatal("unsafe CLI error", e, out.String())
		}
	}
}

func TestCLIImportResumeAndRepeat(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "backup.sqlite")
	dest := filepath.Join(dir, "history.sqlite")
	manifest := filepath.Join(dir, "boundary.json")
	schema, e := os.ReadFile("../../internal/history/cpamp-schema.sql")
	if e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite3", source)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(schema)); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO api_key_aliases VALUES(?, 'CLI alias', 1)`, strings.Repeat("a", 64)); e != nil {
		t.Fatal(e)
	}
	db.Close()
	os.Chmod(source, 0400)
	raw, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(raw)
	b := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hex.EncodeToString(sum[:]), LineageSHA256: strings.Repeat("b", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790899200000}
	raw, e = json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(manifest, raw, 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"--source", source, "--store", dest, "--manifest", manifest, "--frozen", "--batch", "1"}
	var out bytes.Buffer
	if e = run(context.Background(), append(append([]string{}, args...), "--max-batches", "1"), &out); e != nil {
		t.Fatal(e)
	}
	var p history.Progress
	if json.Unmarshal(out.Bytes(), &p) != nil || p.Complete {
		t.Fatal("expected resumable progress")
	}
	for range 2 {
		out.Reset()
		if e = run(context.Background(), args, &out); e != nil {
			t.Fatal(e)
		}
		if json.Unmarshal(out.Bytes(), &p) != nil || !p.Complete || p.Preservation != "archival-only" {
			t.Fatal("missing archive completion")
		}
	}
	s, e := history.Open(dest)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	keys, e := s.Keys(context.Background())
	if e != nil || len(keys.Keys) != 1 || keys.Keys[0].Alias != "CLI alias" {
		t.Fatal(keys, e)
	}
}

func TestCLIStrictManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	dest := filepath.Join(dir, "history.sqlite")
	for _, raw := range []string{`{"unknown":"sk-secret"}`, `{} {}`, `not-json`} {
		if e := os.WriteFile(manifest, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		var out bytes.Buffer
		e := run(context.Background(), []string{"--source", "offline", "--store", dest, "--manifest", manifest, "--frozen"}, &out)
		if !errors.Is(e, history.ErrInput) || out.Len() != 0 {
			t.Fatal(e)
		}
		if _, e = os.Stat(dest); !os.IsNotExist(e) {
			t.Fatal("parsed invalid manifest into store")
		}
	}
}
