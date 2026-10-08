package historyreports

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type ForwardStore struct {
	db        *sql.DB
	reader    *sql.DB
	cursorKey []byte
}

// CheckForwardPath rejects aliases to protected stores before any opening or
// creation. Every existing component must be free of symlinks; hard-link aliases
// to existing protected files are rejected by file identity, not string equality.
func CheckForwardPath(path string, protected ...string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInput
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrInput
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || current == path && !info.Mode().IsRegular()) {
			return ErrInput
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	info, _ := os.Stat(path)
	for _, other := range protected {
		if other == "" {
			continue
		}
		abs, err := filepath.Abs(other)
		if err != nil {
			return ErrInput
		}
		if filepath.Clean(abs) == path {
			return ErrInput
		}
		if otherInfo, err := os.Stat(other); err == nil && info != nil && os.SameFile(info, otherInfo) {
			return ErrInput
		}
	}
	return nil
}

// OpenPreparedForward is the runtime-only opener. SQLite mode=rw atomically
// refuses an absent file. There is no create fallback, schema migration, table
// scan, index build or initializer call, including for an existing empty file.
func OpenPreparedForward(ctx context.Context, path string) (*ForwardStore, error) {
	if err := CheckForwardPath(path); err != nil {
		return nil, ErrInput
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, ErrSource
	}
	db, err := sql.Open("sqlite3", uri(path, "mode=rw&_synchronous=FULL&_busy_timeout=1000"))
	if err != nil {
		return nil, ErrSource
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &ForwardStore{db: db}
	ok := false
	defer func() {
		if !ok {
			if s.reader != nil {
				s.reader.Close()
			}
			db.Close()
		}
	}()
	var version, mode string
	if err = db.QueryRowContext(ctx, `SELECT version,cursor_key FROM forward_meta WHERE id=1`).Scan(&version, &s.cursorKey); err != nil || version != ForwardVersion || len(s.cursorKey) != 32 {
		return nil, ErrSource
	}
	if err = db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		return nil, ErrSource
	}
	for _, name := range []string{"forward_objects", "forward_native", "forward_base_meta", "forward_base_events", "forward_links", "forward_origin_state", "forward_generations"} {
		var n int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			return nil, ErrSource
		}
	}
	for _, name := range []string{"forward_native_physical", "forward_native_page", "forward_base_page", "forward_object_digest"} {
		var n int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			return nil, ErrSource
		}
	}
	for _, query := range []string{
		`SELECT event_id,revision,sequence,sort_key,physical,started,account,provider,credential,body FROM forward_native LIMIT 0`,
		`SELECT digest,kind,name,body FROM forward_objects LIMIT 0`,
		`SELECT id,body,sealed,summary FROM forward_generations LIMIT 0`,
	} {
		rows, e := db.QueryContext(ctx, query)
		if e != nil {
			return nil, ErrSource
		}
		if e = rows.Close(); e != nil {
			return nil, ErrSource
		}
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, ErrSource
	}
	s.reader, err = sql.Open("sqlite3", uri(path, "mode=ro&_busy_timeout=1000"))
	if err != nil {
		return nil, ErrSource
	}
	s.reader.SetMaxOpenConns(2)
	s.reader.SetMaxIdleConns(2)
	if err = s.reader.PingContext(ctx); err != nil {
		return nil, ErrSource
	}
	ok = true
	return s, nil
}

// NativeForwardSelection validates an explicitly authorized immutable book and
// snapshots the local profile branch. It is metadata-only. A store with a frozen
// base requires a separately reviewed migration/runtime selection, never silent
// omission of that base by the native-only publisher.
func (s *ForwardStore) NativeForwardSelection(ctx context.Context, bookDigest string) (ForwardSeal, error) {
	q := ForwardSeal{BookDigest: bookDigest}
	var book ForwardBook
	if err := s.object(ctx, "book", bookDigest, &book); err != nil {
		return q, ErrSource
	}
	if book.Book.Policy != "profile-api-value-v3" || book.ID == "" {
		return q, ErrSource
	}
	var bases int
	if err := s.reader.QueryRowContext(ctx, `SELECT profile_revision,(SELECT count(*) FROM forward_base_meta) FROM forward_meta WHERE id=1`).Scan(&q.ProfileRevision, &bases); err != nil || bases != 0 {
		return q, ErrSource
	}
	if _, _, err := s.ProfilesAt(ctx, q.ProfileRevision); err != nil {
		return q, ErrSource
	}
	return q, nil
}

// PreparedForwardHead reads only the progress row and retained manifest, so a
// runtime can expose the last sealed report while asynchronous adoption resumes.
func (s *ForwardStore) PreparedForwardHead(ctx context.Context) (ForwardStatus, time.Time, error) {
	status, err := s.Status(ctx)
	if err != nil {
		return status, time.Time{}, ErrSource
	}
	if status.Latest == "" {
		return status, time.Time{}, nil
	}
	var body []byte
	var sealed bool
	if err = s.reader.QueryRowContext(ctx, `SELECT body,sealed FROM forward_generations WHERE id=?`, status.Latest).Scan(&body, &sealed); err != nil || !sealed || len(body) > 2<<20 || hash(string(body)) != status.Latest {
		return status, time.Time{}, ErrSource
	}
	var m forwardManifest
	if json.Unmarshal(body, &m) != nil || m.Version != ForwardVersion || m.Converter != ForwardConverterVersion || !validForwardTime(m.At) {
		return status, time.Time{}, ErrSource
	}
	return status, m.At, nil
}

const forwardSchema = `
CREATE TABLE forward_meta(id INTEGER PRIMARY KEY CHECK(id=1),version TEXT NOT NULL,cursor_key BLOB NOT NULL,feed_id TEXT NOT NULL DEFAULT '',through_seq INTEGER NOT NULL DEFAULT 0,bootstrap_through INTEGER NOT NULL DEFAULT 0,profile_revision INTEGER NOT NULL DEFAULT 0,latest TEXT NOT NULL DEFAULT '');
CREATE TABLE forward_objects(digest TEXT NOT NULL,kind TEXT NOT NULL,name TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(kind,name));
CREATE INDEX forward_object_digest ON forward_objects(kind,digest);
CREATE TABLE forward_native(event_id TEXT NOT NULL,revision INTEGER NOT NULL,sequence INTEGER NOT NULL UNIQUE,sort_key TEXT NOT NULL,physical TEXT NOT NULL,started INTEGER NOT NULL,account TEXT NOT NULL,provider TEXT NOT NULL,credential TEXT NOT NULL,body BLOB NOT NULL,PRIMARY KEY(event_id,revision));
CREATE INDEX forward_native_physical ON forward_native(physical,sequence);
CREATE INDEX forward_native_page ON forward_native(started,sort_key,revision);
CREATE TABLE forward_base_meta(id INTEGER PRIMARY KEY CHECK(id=1),digest TEXT NOT NULL,body BLOB NOT NULL,after_hash TEXT NOT NULL DEFAULT '',event_count INTEGER NOT NULL DEFAULT 0,complete INTEGER NOT NULL DEFAULT 0);
CREATE TABLE forward_base_events(id TEXT PRIMARY KEY,event_hash TEXT NOT NULL UNIQUE,account TEXT NOT NULL,provider TEXT NOT NULL,started INTEGER NOT NULL,body BLOB NOT NULL);
CREATE INDEX forward_base_page ON forward_base_events(started,id);
CREATE TABLE forward_links(origin TEXT NOT NULL,baseline TEXT NOT NULL,physical TEXT NOT NULL,account TEXT NOT NULL,provider TEXT NOT NULL,credential TEXT NOT NULL,start_ns INTEGER NOT NULL,end_ns INTEGER NOT NULL,PRIMARY KEY(origin,baseline),UNIQUE(origin,physical));
CREATE TABLE forward_origin_state(digest TEXT PRIMARY KEY,after_id TEXT NOT NULL DEFAULT '',missing_count INTEGER NOT NULL DEFAULT 0,ready INTEGER NOT NULL DEFAULT 0);
CREATE TABLE forward_generations(id TEXT PRIMARY KEY,body BLOB NOT NULL,sealed INTEGER NOT NULL DEFAULT 0,summary BLOB);
`

// OpenForward owns a dedicated local database. Opening scans schema metadata
// only; all adoption/projection/compilation is explicit, cancellable work.
func OpenForward(ctx context.Context, path string) (*ForwardStore, error) {
	if path == "" {
		return nil, ErrInput
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrInput
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		f.Close()
	} else if err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, ErrInput
	}
	db, err := sql.Open("sqlite3", uri(abs, "mode=rw&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &ForwardStore{db: db}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var exists int
		if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='forward_meta' AND type='table'`).Scan(&exists); e != nil {
			return e
		}
		if exists == 0 {
			var n int
			if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table'`).Scan(&n); e != nil {
				return e
			}
			if n != 0 {
				return ErrSource
			}
			if _, e := tx.ExecContext(ctx, forwardSchema); e != nil {
				return e
			}
			key := make([]byte, 32)
			if _, e := rand.Read(key); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO forward_meta(id,version,cursor_key) VALUES(1,?,?)`, ForwardVersion, key); e != nil {
				return e
			}
			initial := ForwardProfileConfig{Profiles: []ForwardProfile{}, Bindings: []ForwardBinding{}}
			body, _ := json.Marshal(initial)
			if _, e := tx.ExecContext(ctx, `INSERT INTO forward_objects VALUES(?,?,?,?)`, hash(string(body)), "profiles", "0", body); e != nil {
				return e
			}
		}
		var version string
		if e := tx.QueryRowContext(ctx, `SELECT version,cursor_key FROM forward_meta WHERE id=1`).Scan(&version, &s.cursorKey); e != nil {
			return e
		}
		if version != ForwardVersion || len(s.cursorKey) != 32 {
			return ErrSource
		}
		for _, name := range []string{"forward_objects", "forward_native", "forward_base_meta", "forward_base_events", "forward_links", "forward_origin_state", "forward_generations"} {
			if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&exists); e != nil {
				return e
			}
			if exists != 1 {
				return ErrSource
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	s.reader, err = sql.Open("sqlite3", uri(abs, "mode=ro&_busy_timeout=5000"))
	if err != nil {
		db.Close()
		return nil, err
	}
	s.reader.SetMaxOpenConns(2)
	s.reader.SetMaxIdleConns(2)
	if err = s.reader.PingContext(ctx); err != nil {
		s.reader.Close()
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *ForwardStore) Close() error { return errors.Join(s.reader.Close(), s.db.Close()) }
func (s *ForwardStore) write(ctx context.Context, fn func(*sql.Tx) error) error {
	// A dedicated connection and a first write make independent Store handles
	// serialize before metadata inspection, without a read-to-write lock upgrade.
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// database/sql Tx uses deferred BEGIN. The first statement acquires the writer
	// via an otherwise harmless PRAGMA user_version assignment, also on first open.
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `PRAGMA user_version=1`); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func forwardObject(tx *sql.Tx, ctx context.Context, kind, name string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", ErrInput
	}
	if len(b) > 2<<20 {
		return "", ErrLimit
	}
	d := hash(string(b))
	var old string
	err = tx.QueryRowContext(ctx, `SELECT digest FROM forward_objects WHERE kind=? AND name=?`, kind, name).Scan(&old)
	if err == nil {
		if old != d {
			return "", ErrForwardConflict
		}
		return d, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO forward_objects(digest,kind,name,body) VALUES(?,?,?,?)`, d, kind, name, b)
	return d, err
}
func (s *ForwardStore) object(ctx context.Context, kind, d string, v any) error {
	var body []byte
	if !validHash(d) {
		return ErrInput
	}
	if err := s.reader.QueryRowContext(ctx, `SELECT body FROM forward_objects WHERE kind=? AND digest=?`, kind, d).Scan(&body); err != nil {
		return err
	}
	if hash(string(body)) != d {
		return ErrForwardConflict
	}
	return json.Unmarshal(body, v)
}

type ForwardStatus struct {
	FeedID           string
	Through          int64
	BootstrapThrough int64
	Latest           string
}

func (s *ForwardStore) Status(ctx context.Context) (v ForwardStatus, err error) {
	err = s.reader.QueryRowContext(ctx, `SELECT feed_id,through_seq,bootstrap_through,latest FROM forward_meta WHERE id=1`).Scan(&v.FeedID, &v.Through, &v.BootstrapThrough, &v.Latest)
	return
}
