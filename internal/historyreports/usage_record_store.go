package historyreports

// The usage record's own SQLite file (usage-record.sqlite, next to Pacer's
// state). It holds:
//   - native evidence indexed by account and time: a copy of the forward
//     ledger's requests and the observation store's meter readings, plus the
//     requests of providers Pacer records only for this page (Antigravity,
//     OpenCode Go, ...), which exist nowhere else;
//   - sealed segments: derived windows and day totals that can no longer change,
//     so a rebuild never derives closed windows again.
// Copied evidence and sealed segments are rebuilt from their sources when the
// file is removed; the usage-only requests are not (see docs/usage-record.md).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
)

const recordSchemaVersion = "1"

const recordSchema = `
CREATE TABLE IF NOT EXISTS record_meta(name TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS record_events(
 id TEXT PRIMARY KEY, revision INTEGER NOT NULL, seq INTEGER NOT NULL UNIQUE,
 ref TEXT NOT NULL, native TEXT NOT NULL, credential TEXT NOT NULL, source TEXT NOT NULL,
 provider TEXT NOT NULL, started INTEGER NOT NULL, origin TEXT NOT NULL, model TEXT NOT NULL, body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS record_events_ref ON record_events(ref, started);
CREATE INDEX IF NOT EXISTS record_events_model ON record_events(model, provider);
CREATE TABLE IF NOT EXISTS record_readings(
 id INTEGER PRIMARY KEY, ref TEXT NOT NULL, native TEXT NOT NULL, provider TEXT NOT NULL,
 kind TEXT NOT NULL, observed INTEGER NOT NULL, body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS record_readings_ref ON record_readings(ref, observed);
CREATE TABLE IF NOT EXISTS record_segments(
 account TEXT NOT NULL, kind TEXT NOT NULL, segment INTEGER NOT NULL, key TEXT NOT NULL,
 body BLOB NOT NULL, PRIMARY KEY(account, kind, segment));
`

// RecordStore is the usage record file. Safe for concurrent use.
type RecordStore struct {
	db   *sql.DB
	path string
}

// OpenRecordStore opens or creates the usage record file (owner-only).
func OpenRecordStore(ctx context.Context, path string) (*RecordStore, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrInput
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, ErrInput
	} else if errors.Is(err, os.ErrNotExist) {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return nil, e
		}
		f.Close()
	}
	db, err := sql.Open("sqlite3", uri(path, "mode=rw&_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err = db.ExecContext(ctx, recordSchema); err != nil {
		db.Close()
		return nil, err
	}
	s := &RecordStore{db: db, path: path}
	if v, ok, err := s.Meta(ctx, "schema"); err != nil {
		db.Close()
		return nil, err
	} else if !ok {
		if err = s.SetMeta(ctx, "schema", recordSchemaVersion); err != nil {
			db.Close()
			return nil, err
		}
	} else if v != recordSchemaVersion {
		db.Close()
		return nil, ErrSource
	}
	return s, nil
}

func (s *RecordStore) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

func (s *RecordStore) Meta(ctx context.Context, name string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM record_meta WHERE name=?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *RecordStore) SetMeta(ctx context.Context, name, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO record_meta(name,value) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, name, value)
	return err
}

// RecordEvent is one native request in the record.
type RecordEvent struct {
	ID         string      `json:"-"`
	Revision   int64       `json:"-"`
	Seq        int64       `json:"-"`
	Ref        string      `json:"-"` // native account reference
	Native     string      `json:"-"` // Pacer's native account ID
	Credential string      `json:"-"`
	Source     string      `json:"-"`
	Provider   string      `json:"-"`
	Started    int64       `json:"-"`
	Origin     string      `json:"-"` // forward or usage
	Event      cpamp.Event `json:"event"`
}

// Changes reports, per native account reference, the earliest evidence time
// that a write added or changed.
type Changes map[string]int64

func (c Changes) add(ref string, at int64) {
	if old, ok := c[ref]; !ok || at < old {
		c[ref] = at
	}
}

// Merge adds other's changes.
func (c Changes) Merge(other Changes) {
	for ref, at := range other {
		c.add(ref, at)
	}
}

// PutEvents stores requests, keeping the highest revision of each ID. Each new
// ID gets the next arrival sequence; a newer revision keeps its sequence.
func (s *RecordStore) PutEvents(ctx context.Context, events []RecordEvent) (Changes, error) {
	out := Changes{}
	if len(events) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var seq int64
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM record_events`).Scan(&seq); err != nil {
		return nil, err
	}
	for _, e := range events {
		body, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		var revision, oldStarted int64
		var oldRef string
		err = tx.QueryRowContext(ctx, `SELECT revision,ref,started FROM record_events WHERE id=?`, e.ID).Scan(&revision, &oldRef, &oldStarted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			seq++
			if _, err = tx.ExecContext(ctx, `INSERT INTO record_events(id,revision,seq,ref,native,credential,source,provider,started,origin,model,body) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.Revision, seq, e.Ref, e.Native, e.Credential, e.Source, e.Provider, e.Started, e.Origin, e.Event.Model, body); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		case e.Revision <= revision:
			continue
		default:
			if _, err = tx.ExecContext(ctx, `UPDATE record_events SET revision=?,ref=?,native=?,credential=?,source=?,provider=?,started=?,origin=?,model=?,body=? WHERE id=?`, e.Revision, e.Ref, e.Native, e.Credential, e.Source, e.Provider, e.Started, e.Origin, e.Event.Model, body, e.ID); err != nil {
				return nil, err
			}
			out.add(oldRef, oldStarted)
		}
		out.add(e.Ref, e.Started)
	}
	return out, tx.Commit()
}

// PutReadings stores meter readings (observation history IDs are unique).
func (s *RecordStore) PutReadings(ctx context.Context, readings []NativeReading, refs []string) (Changes, error) {
	out := Changes{}
	if len(readings) == 0 {
		return out, nil
	}
	if len(refs) != len(readings) {
		return nil, ErrInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i, r := range readings {
		body, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO record_readings(id,ref,native,provider,kind,observed,body) VALUES(?,?,?,?,?,?,?)`, r.ID, refs[i], r.Account, r.Provider, nativeKind(r.Provider, r.Seconds), r.At.UnixMilli(), body)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			out.add(refs[i], r.At.UnixMilli())
		}
	}
	return out, tx.Commit()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// Events reads requests of the given references that started in [from, to),
// ordered by start and arrival.
func (s *RecordStore) Events(ctx context.Context, refs []string, from, to int64) ([]RecordEvent, error) {
	out := []RecordEvent{}
	if len(refs) == 0 || to <= from {
		return out, nil
	}
	args := []any{}
	for _, r := range refs {
		args = append(args, r)
	}
	args = append(args, from, to)
	rows, err := s.db.QueryContext(ctx, `SELECT id,revision,seq,ref,native,credential,source,provider,started,origin,body FROM record_events WHERE ref IN (`+placeholders(len(refs))+`) AND started>=? AND started<? ORDER BY started,seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e RecordEvent
		var body []byte
		if err = rows.Scan(&e.ID, &e.Revision, &e.Seq, &e.Ref, &e.Native, &e.Credential, &e.Source, &e.Provider, &e.Started, &e.Origin, &body); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(body, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Readings reads meter readings of the given references observed in
// [from, to), of the given kinds (nil: all), in ID order.
func (s *RecordStore) Readings(ctx context.Context, refs []string, from, to int64, kinds []string) ([]NativeReading, error) {
	out := []NativeReading{}
	if len(refs) == 0 || to <= from {
		return out, nil
	}
	args := []any{}
	for _, r := range refs {
		args = append(args, r)
	}
	args = append(args, from, to)
	q := `SELECT body FROM record_readings WHERE ref IN (` + placeholders(len(refs)) + `) AND observed>=? AND observed<?`
	if kinds != nil {
		q += ` AND kind IN (` + placeholders(len(kinds)) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		var r NativeReading
		if err = rows.Scan(&body); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RefInfo describes one native account reference in the record.
type RefInfo struct {
	Ref, Native, Provider, Source string
	First, Last                   int64
	Kinds                         []string
}

// Refs lists every native account reference with evidence.
func (s *RecordStore) Refs(ctx context.Context) (map[string]*RefInfo, error) {
	out := map[string]*RefInfo{}
	get := func(ref string) *RefInfo {
		if out[ref] == nil {
			out[ref] = &RefInfo{Ref: ref, Kinds: []string{}}
		}
		return out[ref]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ref,max(native),max(provider),max(source),min(started),max(started) FROM record_events GROUP BY ref`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, native, provider, source string
		var first, last int64
		if err = rows.Scan(&ref, &native, &provider, &source, &first, &last); err != nil {
			rows.Close()
			return nil, err
		}
		i := get(ref)
		i.Native, i.Provider, i.Source, i.First, i.Last = native, provider, source, first, last
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT ref,max(native),max(provider),kind,min(observed),max(observed) FROM record_readings GROUP BY ref,kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref, native, provider, kind string
		var first, last int64
		if err = rows.Scan(&ref, &native, &provider, &kind, &first, &last); err != nil {
			return nil, err
		}
		i := get(ref)
		if i.Native == "" {
			i.Native, i.Provider = native, provider
		}
		if i.First == 0 || first < i.First {
			i.First = first
		}
		i.Last = max(i.Last, last)
		if kind != "" {
			i.Kinds = append(i.Kinds, kind)
		}
	}
	return out, rows.Err()
}

// StoredSegment is one sealed segment as stored.
type StoredSegment struct {
	Kind    string
	Segment int64
	Key     string
	Body    []byte
}

func (s *RecordStore) Segments(ctx context.Context, account string) ([]StoredSegment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind,segment,key,body FROM record_segments WHERE account=?`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StoredSegment{}
	for rows.Next() {
		var g StoredSegment
		if err = rows.Scan(&g.Kind, &g.Segment, &g.Key, &g.Body); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SegmentAccounts lists accounts with sealed segments.
func (s *RecordStore) SegmentAccounts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT account FROM record_segments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err = rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *RecordStore) PutSegments(ctx context.Context, account string, segments []StoredSegment) error {
	if len(segments) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, g := range segments {
		if _, err = tx.ExecContext(ctx, `INSERT INTO record_segments(account,kind,segment,key,body) VALUES(?,?,?,?,?) ON CONFLICT(account,kind,segment) DO UPDATE SET key=excluded.key,body=excluded.body`, account, g.Kind, g.Segment, g.Key, g.Body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DropSegments removes an account's sealed segments (all when kind is "*").
func (s *RecordStore) DropSegments(ctx context.Context, account string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM record_segments WHERE account=?`, account)
	return err
}

// Stats is a size summary for the page's technical details.
type RecordStoreStats struct {
	Events   int64 `json:"events"`
	Readings int64 `json:"readings"`
	Segments int64 `json:"sealed_segments"`
}

func (s *RecordStore) Stats(ctx context.Context) (RecordStoreStats, error) {
	var st RecordStoreStats
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM record_events),(SELECT count(*) FROM record_readings),(SELECT count(*) FROM record_segments)`).Scan(&st.Events, &st.Readings, &st.Segments)
	return st, err
}

// RecordEventFromForward is a forward ledger request as a record request.
func RecordEventFromForward(e ForwardEvent) RecordEvent {
	return RecordEvent{ID: e.ID, Revision: e.Revision, Ref: ForwardAccountRef(e.SourceID, e.Event.Provider, e.Event.Account), Native: e.Event.Account, Credential: e.Attribution.CredentialRef, Source: e.SourceID, Provider: e.Event.Provider, Started: e.StartedAt.UnixMilli(), Origin: "forward", Event: e.Event}
}

// UsageOnlyEvent is a final request of a provider Pacer records only for the
// usage page, converted as forward requests are.
func UsageOnlyEvent(r measurement.Record, received time.Time) RecordEvent {
	f := convertForward(measurement.ReportFeedEntry{StoredRecord: measurement.StoredRecord{Record: r, ReceivedAt: received}})
	e := RecordEventFromForward(f)
	e.Origin = "usage"
	if e.Revision <= 0 {
		e.Revision = 1
	}
	return e
}

// Models lists every model of the record's requests with its provider.
func (s *RecordStore) Models(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model,provider FROM record_events`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, p string
		if err = rows.Scan(&m, &p); err != nil {
			return nil, err
		}
		if m != "" {
			out[m] = p
		}
	}
	return out, rows.Err()
}

// PriceSync is the last models.dev sync, kept in the record file.
type PriceSync struct {
	Catalog    ModelsDevCatalog `json:"catalog,omitempty"`
	SyncedAtMS int64            `json:"synced_at_ms,omitempty"` // last successful fetch
	CheckedMS  int64            `json:"checked_at_ms,omitempty"`
	ETag       string           `json:"etag,omitempty"`
	Error      string           `json:"error,omitempty"`
	Models     int              `json:"models"`
}

func (s *RecordStore) PriceSync(ctx context.Context) (PriceSync, error) {
	var out PriceSync
	raw, ok, err := s.Meta(ctx, "models_dev")
	if err != nil || !ok {
		return out, err
	}
	err = json.Unmarshal([]byte(raw), &out)
	return out, err
}

func (s *RecordStore) SetPriceSync(ctx context.Context, v PriceSync) error {
	v.Models = 0
	for _, m := range v.Catalog {
		v.Models += len(m)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetMeta(ctx, "models_dev", string(b))
}
