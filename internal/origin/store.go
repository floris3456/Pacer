// Package origin keeps account ownership of opaque Responses state separately
// from Pacer's main state database. It stores keyed digests, never response bodies.
package origin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrInvalid     = errors.New("invalid opaque Responses state")
	ErrUnknown     = errors.New("opaque Responses origin is unknown or expired")
	ErrMismatch    = errors.New("opaque Responses origin differs from the selected account")
	ErrUntrusted   = errors.New("opaque Responses origin requires completed provider evidence")
	ErrUnavailable = errors.New("opaque Responses origin storage is unavailable")
)

const retention = 30 * 24 * time.Hour
const maxRecords = 100000

// Binding is supplied by the physical host executor. Identity is a digest of
// its effective account and executor endpoint, not a client header or alias.
type Binding struct {
	Caller, AuthID, Identity string
}

type Store struct {
	db  *sql.DB
	key [32]byte
	now func() time.Time
}

// Open creates an owner-only sidecar directory and database. No DDL runs in the
// main state file, whose schema cookie is part of the measurement contract.
func Open(statePath string, now func() time.Time) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		return nil, ErrUnavailable
	}
	statePath, err := filepath.Abs(statePath)
	if err != nil {
		return nil, ErrUnavailable
	}
	dir := statePath + ".codex-origin"
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, ErrUnavailable
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrUnavailable
	}
	path := filepath.Join(dir, "origins.sqlite")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		err = file.Close()
	} else if errors.Is(err, os.ErrExist) {
		info, err = os.Lstat(path)
		if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
			err = ErrUnavailable
		}
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?_busy_timeout=5000&_journal_mode=DELETE&_synchronous=FULL")
	if err != nil {
		return nil, ErrUnavailable
	}
	db.SetMaxOpenConns(1)
	if now == nil {
		now = time.Now
	}
	s := &Store{db: db, now: now}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, ErrUnavailable
	}
	return s, nil
}

func (s *Store) init() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS origin_key (id INTEGER PRIMARY KEY CHECK (id=1), key BLOB NOT NULL CHECK(length(key)=32)); CREATE TABLE IF NOT EXISTS origins (marker TEXT PRIMARY KEY, auth_id TEXT NOT NULL, binding TEXT NOT NULL, expires_ms INTEGER NOT NULL); CREATE INDEX IF NOT EXISTS origins_expiry ON origins(expires_ms)`); err != nil {
		return err
	}
	var key []byte
	err = tx.QueryRow("SELECT key FROM origin_key WHERE id=1").Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO origin_key(id,key) VALUES(1,?)", key)
	}
	if err != nil || len(key) != 32 {
		return ErrUnavailable
	}
	copy(s.key[:], key)
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func safeText(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func (s *Store) digest(domain string, values ...string) string {
	mac := hmac.New(sha256.New, s.key[:])
	body, _ := json.Marshal(append([]string{"pacer-codex-origin-v1", domain}, values...))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) marker(caller string, m marker) string {
	return s.digest("marker", caller, m.kind, m.value)
}
func (s *Store) binding(b Binding) string { return s.digest("binding", b.Caller, b.AuthID, b.Identity) }

func HasOpaqueRequest(body []byte) (bool, error) {
	markers, err := requestMarkers(body)
	return len(markers) > 0, err
}

// Owner returns only the common credential ID, for intersection with the
// caller's authorized pool. It grants no permission and never pins plaintext.
func (s *Store) Owner(ctx context.Context, caller string, body []byte) (string, error) {
	marks, err := requestMarkers(body)
	if err != nil {
		return "", err
	}
	if len(marks) == 0 {
		return "", nil
	}
	if s == nil || !safeText(caller, 512) {
		return "", ErrUnavailable
	}
	var owner, binding string
	for _, m := range marks {
		var id, digest string
		var expires int64
		err := s.db.QueryRowContext(ctx, "SELECT auth_id,binding,expires_ms FROM origins WHERE marker=?", s.marker(caller, m)).Scan(&id, &digest, &expires)
		if errors.Is(err, sql.ErrNoRows) || err == nil && expires <= s.now().UnixMilli() {
			return "", ErrUnknown
		}
		if err != nil || !safeText(id, 512) || len(digest) != 64 {
			return "", ErrUnavailable
		}
		if owner != "" && (owner != id || binding != digest) {
			return "", ErrMismatch
		}
		owner, binding = id, digest
	}
	return owner, nil
}

// Check is repeated against the actual final wire and actual executor identity.
func (s *Store) Check(ctx context.Context, b Binding, body []byte) error {
	marks, err := requestMarkers(body)
	if err != nil {
		return err
	}
	if len(marks) == 0 {
		return nil
	}
	if s == nil || !validBinding(b) {
		return ErrUnavailable
	}
	want := s.binding(b)
	for _, m := range marks {
		var got string
		var expires int64
		err := s.db.QueryRowContext(ctx, "SELECT binding,expires_ms FROM origins WHERE marker=?", s.marker(b.Caller, m)).Scan(&got, &expires)
		if errors.Is(err, sql.ErrNoRows) || err == nil && expires <= s.now().UnixMilli() {
			return ErrUnknown
		}
		if err != nil {
			return ErrUnavailable
		}
		if !hmac.Equal([]byte(got), []byte(want)) {
			return ErrMismatch
		}
	}
	return nil
}

func validBinding(b Binding) bool {
	return safeText(b.Caller, 512) && safeText(b.AuthID, 512) && safeText(b.Identity, 512)
}

// Observe requires the successful physical transport's terminal response. The
// whole marker batch commits before the successful terminal response is released.
func (s *Store) Observe(ctx context.Context, b Binding, body []byte) error {
	if s == nil || !validBinding(b) {
		return ErrUnavailable
	}
	marks, err := responseMarkers(body)
	if err != nil {
		return err
	}
	if len(marks) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UnixMilli()
	if _, err = tx.ExecContext(ctx, "DELETE FROM origins WHERE expires_ms<=?", now); err != nil {
		return ErrUnavailable
	}
	want := s.binding(b)
	for _, m := range marks {
		key := s.marker(b.Caller, m)
		var got string
		err = tx.QueryRowContext(ctx, "SELECT binding FROM origins WHERE marker=?", key).Scan(&got)
		if err == nil && !hmac.Equal([]byte(got), []byte(want)) {
			return ErrMismatch
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ErrUnavailable
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO origins(marker,auth_id,binding,expires_ms) VALUES(?,?,?,?) ON CONFLICT(marker) DO UPDATE SET expires_ms=excluded.expires_ms`, key, b.AuthID, want, now+retention.Milliseconds()); err != nil {
			return ErrUnavailable
		}
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM origins").Scan(&count); err != nil || count > maxRecords {
		return ErrUnavailable
	}
	if err = tx.Commit(); err != nil {
		return ErrUnavailable
	}
	return nil
}
