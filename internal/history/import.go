package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// ImportOptions is an offline-only capability. Neither source path nor this
// operation belongs on a management HTTP surface. Each committed batch has at
// most BatchSize rows (1..1000) and a 4MiB soft byte limit (16MiB max single row).
// MaxBatches=0 runs to completion; positive values allow cooperative scheduling.
type ImportOptions struct {
	SourcePath     string
	ReportPinsPath string
	Boundary       Boundary
	BatchSize      int
	MaxBatches     int
}

func (s *Store) Import(ctx context.Context, o ImportOptions) (Progress, error) {
	if !o.Boundary.valid() || o.BatchSize < 1 || o.BatchSize > 1000 || o.MaxBatches < 0 {
		return Progress{}, ErrInput
	}
	pins, e := readPins(o.ReportPinsPath, o.Boundary.ReportPinsSHA256)
	if e != nil {
		return Progress{}, e
	}
	f, e := openFrozen(o.SourcePath, o.Boundary)
	if e != nil {
		return Progress{}, e
	}
	defer f.db.Close()
	dest, e := os.Stat(s.path)
	if e != nil || os.SameFile(dest, f.info) {
		return Progress{}, ErrInput
	}
	objects, tables, e := inspect(ctx, f, o.Boundary)
	if e != nil {
		return Progress{}, e
	}
	if e = s.initialize(ctx, f, o.Boundary, objects, tables, pins); e != nil {
		return Progress{}, e
	}
	for batches := 0; o.MaxBatches == 0 || batches < o.MaxBatches; batches++ {
		if ctx.Err() != nil {
			return Progress{}, ctx.Err()
		}
		if e = f.unchanged(); e != nil {
			return Progress{}, e
		}
		complete, e := s.copyBatch(ctx, f, o.BatchSize)
		if e != nil {
			return Progress{}, e
		}
		if complete {
			// A source modified during staging can never publish mixed history.
			digest, _, e := frozenDigest(f.path)
			if e != nil {
				return Progress{}, e
			}
			if digest != o.Boundary.SnapshotSHA256 {
				return Progress{}, ErrDigest
			}
			if e = s.publish(ctx); e != nil {
				return Progress{}, e
			}
			break
		}
	}
	return s.Status(ctx)
}

func readPins(path, digest string) ([]byte, error) {
	if path == "" && digest == "" {
		return nil, nil
	}
	if path == "" || !validHash(digest) {
		return nil, ErrInput
	}
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 || info.Size() > 16<<20 {
		return nil, ErrSource
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrSource
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if e != nil || len(raw) > 16<<20 || !json.Valid(raw) {
		return nil, ErrSource
	}
	if hash(raw) != digest {
		return nil, ErrDigest
	}
	return raw, nil
}

func (s *Store) initialize(ctx context.Context, f *frozen, b Boundary, objects []schemaObject, tables []tableSpec, pins []byte) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	var prior []byte
	e = tx.QueryRowContext(ctx, `SELECT boundary FROM history_import WHERE id=1`).Scan(&prior)
	if e == nil {
		if !bytes.Equal(prior, encode(b)) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return ErrStorage
	}
	var revisions int
	if tx.QueryRowContext(ctx, `SELECT count(*) FROM history_metadata`).Scan(&revisions) != nil {
		return ErrStorage
	}
	if revisions != 0 {
		return ErrConflict
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO history_import(id,boundary) VALUES(1,?)`, encode(b)); e != nil {
		return ErrStorage
	}
	for _, name := range []string{"user_version", "application_id", "encoding"} {
		var value string
		if f.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&value) != nil {
			return ErrSource
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO history_source(name,value) VALUES(?,?)`, name, value); e != nil {
			return ErrStorage
		}
	}
	if pins != nil {
		if _, e = tx.ExecContext(ctx, `INSERT INTO history_artifacts(name,digest,payload) VALUES('report-pins',?,?)`, b.ReportPinsSHA256, pins); e != nil {
			return ErrStorage
		}
	}
	for _, o := range objects {
		if _, e = tx.ExecContext(ctx, `INSERT INTO history_schema(kind,name,tbl,sql) VALUES(?,?,?,?)`, o.Kind, o.Name, o.Table, o.SQL); e != nil {
			return ErrStorage
		}
	}
	for _, t := range tables {
		if _, e = tx.ExecContext(ctx, `INSERT INTO history_tables(name,spec,complete) VALUES(?,?,?)`, t.Name, encode(t), t.Virtual); e != nil {
			return ErrStorage
		}
	}
	if tx.Commit() != nil {
		return ErrStorage
	}
	return nil
}

func (s *Store) copyBatch(ctx context.Context, f *frozen, limit int) (bool, error) {
	// Serialized with all metadata writes and other import processes. Source
	// reads use a separate immutable handle; target rollback cannot affect it.
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return false, ErrStorage
	}
	defer tx.Rollback()
	var spec, cur []byte
	var count int64
	var chain string
	e = tx.QueryRowContext(ctx, `SELECT spec,cursor,rows,chain FROM history_tables WHERE complete=0 ORDER BY name LIMIT 1`).Scan(&spec, &cur, &count, &chain)
	if errors.Is(e, sql.ErrNoRows) {
		return true, nil
	}
	if e != nil {
		return false, ErrStorage
	}
	var t tableSpec
	var cursor []Cell
	if json.Unmarshal(spec, &t) != nil || len(cur) > 0 && json.Unmarshal(cur, &cursor) != nil {
		return false, ErrStorage
	}
	batch, e := readBatch(ctx, f, t, cursor, limit)
	if e != nil {
		return false, e
	}
	for _, r := range batch {
		count++
		if _, e = tx.ExecContext(ctx, `INSERT INTO history_rows(tbl,ordinal,key,cells) VALUES(?,?,?,?)`, t.Name, count, encode(r.Key), encode(r.Cells)); e != nil {
			return false, ErrStorage
		}
		if e = projectRow(ctx, tx, t, r); e != nil {
			return false, e
		}
		cursor = r.Key
		chain = chainHash(chain, r)
	}
	if e = f.unchanged(); e != nil {
		return false, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE history_tables SET cursor=?,rows=?,chain=?,complete=? WHERE name=?`, encode(cursor), count, chain, len(batch) == 0, t.Name); e != nil {
		return false, ErrStorage
	}
	if tx.Commit() != nil {
		return false, ErrStorage
	}
	return false, nil
}

func projectRow(ctx context.Context, tx *sql.Tx, t tableSpec, r archiveRow) error {
	m := rowMap(t, r)
	switch t.Name {
	case "api_key_aliases", "api_key_alias_history", "analytics_event_keys", "usage_events":
		key := m["api_key_hash"].text()
		if key == "" && (t.Name == "usage_events" || t.Name == "analytics_event_keys") {
			return nil
		}
		if !validHash(key) {
			return ErrSource
		}
		if _, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_key_stage(hash) VALUES(?)`, key); e != nil {
			return ErrStorage
		}
		if t.Name == "api_key_aliases" {
			if _, e := tx.ExecContext(ctx, `UPDATE history_key_stage SET alias=? WHERE hash=?`, m["alias"].text(), key); e != nil {
				return ErrStorage
			}
		}
		if t.Name == "api_key_alias_history" {
			owner := m["current_hash"].text()
			if !validHash(owner) || key == owner {
				return ErrSource
			}
			if _, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_key_stage(hash) VALUES(?)`, owner); e != nil {
				return ErrStorage
			}
			if _, e := tx.ExecContext(ctx, `UPDATE history_key_stage SET owner=? WHERE hash=?`, owner, key); e != nil {
				return ErrStorage
			}
		}
	case "analytics_events":
		// Legacy analytics can retain identities absent from the raw event ledger.
		var v struct {
			Key string `json:"api_key_hash"`
		}
		if json.Unmarshal([]byte(m["payload"].text()), &v) != nil {
			return ErrSource
		}
		if v.Key != "" {
			if !validHash(v.Key) {
				return ErrSource
			}
			if _, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO history_key_stage(hash) VALUES(?)`, v.Key); e != nil {
				return ErrStorage
			}
		}
	case "analytics_profile_revisions":
		var c sourceProfiles
		if json.Unmarshal([]byte(m["payload"].text()), &c) != nil {
			return ErrSource
		}
		n, e := m["revision"].value()
		if e != nil || n != c.Revision {
			return ErrConflict
		}
	}
	return nil
}

func (s *Store) publish(ctx context.Context) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ErrStorage
	}
	defer tx.Rollback()
	var complete bool
	if tx.QueryRowContext(ctx, `SELECT complete FROM history_import WHERE id=1`).Scan(&complete) != nil {
		return ErrStorage
	}
	if complete {
		return nil
	}
	var pending int
	if tx.QueryRowContext(ctx, `SELECT count(*) FROM history_tables WHERE complete=0`).Scan(&pending) != nil {
		return ErrStorage
	}
	if pending != 0 {
		return ErrPending
	}
	rows, e := tx.QueryContext(ctx, `SELECT hash,owner,alias FROM history_key_stage ORDER BY hash`)
	if e != nil {
		return ErrStorage
	}
	owners := map[string]string{}
	labels := map[string]string{}
	for rows.Next() {
		var key, owner, label string
		if rows.Scan(&key, &owner, &label) != nil {
			rows.Close()
			return ErrStorage
		}
		owners[key] = owner
		labels[key] = label
		if len(owners) > 10000 {
			rows.Close()
			return ErrSource
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return ErrStorage
	}
	c := Metadata{Revision: 1, Keys: []Key{}, Groups: []Group{}}
	lineages := map[string][]string{}
	for key, owner := range owners {
		if owner != "" {
			lineages[owner] = append(lineages[owner], key)
		}
	}
	for key, owner := range owners {
		if owner != "" {
			target, ok := owners[owner]
			if !ok || target != "" || labels[key] != "" {
				return ErrSource
			}
			continue
		}
		k := Key{Reference: "key:" + key, CurrentHash: key, Alias: safeLabel(labels[key]), Lineage: []string{key}, Imported: true}
		k.Lineage = append(k.Lineage, lineages[key]...)
		c.Keys = append(c.Keys, k)
	}
	normalizeMetadata(&c)
	if _, e = tx.ExecContext(ctx, `INSERT INTO history_metadata(revision,payload) VALUES(1,?)`, encode(c)); e != nil {
		return ErrConflict
	}
	if _, e = tx.ExecContext(ctx, `UPDATE history_import SET complete=1 WHERE id=1`); e != nil {
		return ErrStorage
	}
	if tx.Commit() != nil {
		return ErrStorage
	}
	return nil
}
