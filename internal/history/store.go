// Package history owns Pacer's offline CPAMP archive and display-only metadata.
// It has no authorization, queue-consumption, routing, or report-calculation role.
package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrInput    = errors.New("history: invalid input")
	ErrSource   = errors.New("history: incompatible or non-frozen source")
	ErrDigest   = errors.New("history: snapshot digest mismatch")
	ErrConflict = errors.New("history: immutable boundary or metadata revision conflict")
	ErrPending  = errors.New("history: archive not published")
	ErrStorage  = errors.New("history: storage operation failed")
)

type Store struct {
	db   *sql.DB
	path string
}

const storeSchema = `
CREATE TABLE history_meta (version INTEGER NOT NULL);
INSERT INTO history_meta VALUES(1);
CREATE TABLE history_import (id INTEGER PRIMARY KEY CHECK(id=1), boundary BLOB NOT NULL, complete INTEGER NOT NULL DEFAULT 0);
CREATE TABLE history_source (name TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE history_artifacts (name TEXT PRIMARY KEY, digest TEXT NOT NULL, payload BLOB NOT NULL);
CREATE TABLE history_schema (kind TEXT NOT NULL, name TEXT NOT NULL, tbl TEXT NOT NULL, sql TEXT, PRIMARY KEY(kind,name));
CREATE TABLE history_tables (name TEXT PRIMARY KEY, spec BLOB NOT NULL, cursor BLOB, rows INTEGER NOT NULL DEFAULT 0, chain TEXT NOT NULL DEFAULT '', complete INTEGER NOT NULL DEFAULT 0);
CREATE TABLE history_rows (tbl TEXT NOT NULL, ordinal INTEGER NOT NULL, key BLOB NOT NULL, cells BLOB NOT NULL, PRIMARY KEY(tbl,ordinal), UNIQUE(tbl,key));
CREATE TABLE history_key_stage (hash TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', alias TEXT NOT NULL DEFAULT '');
CREATE TABLE history_metadata (revision INTEGER PRIMARY KEY, payload BLOB NOT NULL);
`

// Open opens only a dedicated Pacer history DB. Existing foreign databases are
// rejected before any writable connection/PRAGMA is used. The parent must keep
// this path outside the scheduler's runtime DB and off management input surfaces.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrInput
	}
	info, err := os.Lstat(abs)
	fresh := errors.Is(err, os.ErrNotExist)
	if !fresh {
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, ErrInput
		}
		ro, e := sql.Open("sqlite3", dsn(abs, "mode=ro&_query_only=1"))
		if e != nil {
			return nil, ErrStorage
		}
		var version int
		e = ro.QueryRow(`SELECT version FROM history_meta`).Scan(&version)
		ro.Close()
		if e != nil || version != 1 {
			return nil, ErrConflict
		}
	} else {
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, ErrStorage
		}
		f.Close()
	}
	db, err := sql.Open("sqlite3", dsn(abs, "mode=rw&_busy_timeout=5000&_txlock=immediate&_journal_mode=WAL&_synchronous=FULL"))
	if err != nil {
		return nil, ErrStorage
	}
	db.SetMaxOpenConns(1)
	if fresh {
		var tx *sql.Tx
		tx, err = db.Begin()
		if err == nil {
			_, err = tx.Exec(storeSchema)
			if err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
	} else {
		err = db.Ping()
	}
	if err != nil {
		db.Close()
		return nil, ErrStorage
	}
	return &Store{db: db, path: abs}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func dsn(path, options string) string {
	return (&url.URL{Scheme: "file", Path: path}).String() + "?" + options
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func encode(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}

// Boundary is supplied by a trusted offline operator, never inferred from now,
// an event timestamp, filename, or current source installation. LineageSHA256
// identifies the operator's CPAMP installation identity record, not a credential.
type Boundary struct {
	Schema              string `json:"schema"`
	SnapshotSHA256      string `json:"snapshot_sha256"`
	LineageSHA256       string `json:"lineage_sha256"`
	CheckpointSHA256    string `json:"checkpoint_sha256"`
	ReportPinsSHA256    string `json:"report_pins_sha256,omitempty"`
	HMS                 int64  `json:"h_ms"`
	EventsThrough       int64  `json:"events_through"`
	ObservationsThrough int64  `json:"observations_through"`
}

func (b Boundary) valid() bool {
	return (b.Schema == "cpamp-usage-v2" || b.Schema == "cpamp-usage-v3") && validHash(b.SnapshotSHA256) && validHash(b.LineageSHA256) && validHash(b.CheckpointSHA256) && (b.ReportPinsSHA256 == "" || validHash(b.ReportPinsSHA256)) && b.HMS > 0 && b.EventsThrough >= 0 && b.ObservationsThrough >= 0
}

type TableProgress struct {
	Reference   string `json:"reference"`
	Rows        int64  `json:"rows"`
	ChainSHA256 string `json:"chain_sha256"`
	Complete    bool   `json:"complete"`
}
type Progress struct {
	Boundary     Boundary        `json:"boundary"`
	Complete     bool            `json:"complete"`
	Preservation string          `json:"preservation"`
	Tables       []TableProgress `json:"tables"`
}

// Status is safe for management: no source paths, schema SQL, payloads, table
// names, or source error text. Completion means archival preservation ONLY.
func (s *Store) Status(ctx context.Context) (Progress, error) {
	p := Progress{Preservation: "archival-only", Tables: []TableProgress{}}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT boundary,complete FROM history_import WHERE id=1`).Scan(&raw, &p.Complete); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, ErrPending
		}
		return p, ErrStorage
	}
	if json.Unmarshal(raw, &p.Boundary) != nil {
		return p, ErrStorage
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name,rows,chain,complete FROM history_tables ORDER BY name`)
	if err != nil {
		return p, ErrStorage
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var t TableProgress
		if rows.Scan(&name, &t.Rows, &t.ChainSHA256, &t.Complete) != nil {
			return p, ErrStorage
		}
		t.Reference = "table:" + hash([]byte(name))
		p.Tables = append(p.Tables, t)
	}
	if rows.Err() != nil {
		return p, ErrStorage
	}
	return p, nil
}
