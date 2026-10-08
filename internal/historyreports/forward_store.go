package historyreports

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type ForwardStore struct {
	db        *sql.DB
	reader    *sql.DB
	cursorKey []byte
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
