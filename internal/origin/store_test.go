package origin

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkpoint(cipher string) []byte {
	return []byte(`{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"` + cipher + `"}]}`)
}
func replay(cipher string) []byte {
	return []byte(`{"input":[{"type":"compaction","encrypted_content":"` + cipher + `"}]}`)
}
func testStore(t *testing.T) (*Store, string, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "main.sqlite")
	s, err := Open(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path, &now
}

func TestOriginDurablePrivateSidecarLeavesMainSchemaUntouched(t *testing.T) {
	s, path, now := testStore(t)
	main, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = main.Close() }()
	if _, err = main.Exec("PRAGMA schema_version=70"); err != nil {
		t.Fatal(err)
	}
	b := Binding{Caller: "caller", AuthID: "credential-a", Identity: "account-and-executor-a"}
	cipher := strings.Repeat("SECRET-OPAQUE-STATE", 128)
	if err = s.Observe(context.Background(), b, checkpoint(cipher)); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.Check(context.Background(), b, replay(cipher)); err != nil {
		t.Fatal(err)
	}
	owner, err := s.Owner(context.Background(), b.Caller, replay(cipher))
	if err != nil || owner != b.AuthID {
		t.Fatalf("owner=%q err=%v", owner, err)
	}
	var cookie int
	if err = main.QueryRow("PRAGMA schema_version").Scan(&cookie); err != nil || cookie != 70 {
		t.Fatalf("schema cookie=%d err=%v", cookie, err)
	}
	files, err := os.ReadDir(path + ".codex-origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("public sidecar mode %v", info.Mode())
		}
		data, err := os.ReadFile(filepath.Join(path+".codex-origin", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(cipher)) || bytes.Contains(data, []byte(b.Identity)) || bytes.Contains(data, []byte(b.Caller)) {
			t.Fatal("raw origin material persisted")
		}
	}
}

func TestOriginUnknownCallerAccountExecutorAndExpiryFailClosed(t *testing.T) {
	s, _, now := testStore(t)
	b := Binding{Caller: "caller", AuthID: "a", Identity: "account-a/executor-a"}
	ctx := context.Background()
	if err := s.Check(ctx, b, replay("unknown")); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if err := s.Observe(ctx, b, checkpoint("cipher")); err != nil {
		t.Fatal(err)
	}
	for _, other := range []Binding{{Caller: "other", AuthID: "a", Identity: b.Identity}, {Caller: b.Caller, AuthID: "b", Identity: b.Identity}, {Caller: b.Caller, AuthID: "a", Identity: "account-b/executor-a"}, {Caller: b.Caller, AuthID: "a", Identity: "account-a/executor-b"}} {
		if err := s.Check(ctx, other, replay("cipher")); err == nil {
			t.Fatalf("accepted different origin %+v", other)
		}
	}
	*now = now.Add(retention)
	if err := s.Check(ctx, b, replay("cipher")); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, b, replay("cipher")); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestOriginOnlyCompletedEvidenceAndAtomicConflict(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	a := Binding{Caller: "caller", AuthID: "a", Identity: "one"}
	b := Binding{Caller: "caller", AuthID: "b", Identity: "two"}
	for _, body := range []string{`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"reasoning","encrypted_content":"bad"}]}}`, `{"object":"response.compaction","error":{"code":"failed"},"output":[{"type":"compaction","encrypted_content":"bad"}]}`, `{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","status":"in_progress","encrypted_content":"bad"}]}}`} {
		if err := s.Observe(ctx, a, []byte(body)); err == nil {
			t.Fatal("uncompleted evidence accepted")
		}
	}
	if err := s.Observe(ctx, a, checkpoint("old")); err != nil {
		t.Fatal(err)
	}
	conflict := []byte(`{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"new"},{"type":"compaction","encrypted_content":"old"}]}`)
	if err := s.Observe(ctx, b, conflict); !errors.Is(err, ErrMismatch) {
		t.Fatal(err)
	}
	if err := s.Check(ctx, b, replay("new")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("batch not atomic: %v", err)
	}
}

func TestOriginOpaqueShapesAndOrdinaryPlaintext(t *testing.T) {
	plain := []string{`{"input":"hello"}`, `{"input":[{"type":"message","id":"copied-message","role":"assistant","content":"hello"}]}`, `{"input":"{\"encrypted_content\":\"ordinary text\"}","tools":[{"parameters":{"properties":{"encrypted_content":{"type":"string"}}}}]}`}
	for _, body := range plain {
		if opaque, err := HasOpaqueRequest([]byte(body)); opaque || err != nil {
			t.Fatalf("plaintext pinned %s: %v", body, err)
		}
		var unavailable *Store
		if err := unavailable.Check(context.Background(), Binding{}, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{"input":[{"type":"reasoning","encrypted_content":"cipher"}]}`, `{"previous_response_id":"response"}`, `{"input":[{"type":"item_reference","id":"reasoning"}]}`, `{"conversation":{"id":"conversation"}}`} {
		if opaque, err := HasOpaqueRequest([]byte(body)); !opaque || err != nil {
			t.Fatalf("missed opaque %s: %v", body, err)
		}
	}
	for _, body := range []string{`{"input":[],"input":[{"type":"compaction","encrypted_content":"cipher"}]}`, `{"input":[{"type":"compaction"}]}`, `{"input":[{"type":"message","encrypted_content":"cipher"}]}`, `{"encrypted_content":"cipher"}`, `{"input":[{"type":"compaction","encrypted_content":null}]}`} {
		if _, err := HasOpaqueRequest([]byte(body)); err == nil {
			t.Fatalf("ambiguous state accepted %s", body)
		}
	}
}

func TestOriginMixedOwnersAndReasoningReferences(t *testing.T) {
	s, _, _ := testStore(t)
	ctx := context.Background()
	a := Binding{Caller: "caller", AuthID: "a", Identity: "one"}
	b := Binding{Caller: "caller", AuthID: "b", Identity: "two"}
	if err := s.Observe(ctx, a, []byte(`{"type":"response.completed","response":{"id":"response-a","status":"completed","output":[{"type":"reasoning","id":"reasoning-a","encrypted_content":"cipher-a"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(ctx, b, checkpoint("cipher-b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, a, []byte(`{"previous_response_id":"response-a","input":[{"type":"reasoning","id":"reasoning-a","encrypted_content":"cipher-a"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Owner(ctx, "caller", []byte(`{"input":[{"type":"compaction","encrypted_content":"cipher-a"},{"type":"compaction","encrypted_content":"cipher-b"}]}`)); !errors.Is(err, ErrMismatch) {
		t.Fatal(err)
	}
}

func TestOriginRejectsPublicOrSymlinkStore(t *testing.T) {
	for _, kind := range []string{"public-directory", "symlink-file"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "main.sqlite")
			if err := os.Mkdir(path+".codex-origin", 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "public-directory" {
				if err := os.Chmod(path+".codex-origin", 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(path, filepath.Join(path+".codex-origin", "origins.sqlite")); err != nil {
					t.Fatal(err)
				}
			}
			if s, err := Open(path, nil); err == nil {
				_ = s.Close()
				t.Fatal("unsafe sidecar opened")
			}
		})
	}
}
