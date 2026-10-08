package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Cell preserves SQLite storage classes, including invalid UTF-8 TEXT, BLOB,
// NULL, exact signed integers and IEEE754 REAL bits. Data is never an API DTO.
type Cell struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

func cell(kind string, value any) (Cell, error) {
	c := Cell{Type: kind}
	switch kind {
	case "null":
	case "integer":
		v, ok := value.(int64)
		if !ok {
			return c, ErrSource
		}
		c.Data = strconv.FormatInt(v, 10)
	case "real":
		v, ok := value.(float64)
		if !ok {
			return c, ErrSource
		}
		c.Data = strconv.FormatUint(math.Float64bits(v), 16)
	case "text", "blob":
		v, ok := value.([]byte)
		if !ok {
			return c, ErrSource
		}
		c.Data = base64.StdEncoding.EncodeToString(v)
	default:
		return c, ErrSource
	}
	return c, nil
}
func (c Cell) value() (any, error) {
	switch c.Type {
	case "null":
		return nil, nil
	case "integer":
		return strconv.ParseInt(c.Data, 10, 64)
	case "real":
		v, e := strconv.ParseUint(c.Data, 16, 64)
		return math.Float64frombits(v), e
	case "text", "blob":
		b, e := base64.StdEncoding.DecodeString(c.Data)
		if c.Type == "text" {
			return string(b), e
		}
		return b, e
	}
	return nil, ErrSource
}
func (c Cell) text() string {
	v, e := c.value()
	if e != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

type tableSpec struct {
	Name    string
	Columns []string
	Keys    []string
	Virtual bool
}
type schemaObject struct {
	Kind, Name, Table string
	SQL               sql.NullString
}
type frozen struct {
	db   *sql.DB
	path string
	info os.FileInfo
}

func frozenDigest(path string) (string, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
		return "", nil, ErrSource
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e := os.Lstat(path + suffix); !os.IsNotExist(e) {
			return "", nil, ErrSource
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", nil, ErrSource
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", nil, ErrSource
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", nil, ErrSource
	}
	after, err := f.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return "", nil, ErrSource
	}
	return hex.EncodeToString(h.Sum(nil)), info, nil
}
func openFrozen(path string, b Boundary) (*frozen, error) {
	abs, err := filepath.Abs(path)
	if err != nil || !b.valid() {
		return nil, ErrInput
	}
	digest, info, err := frozenDigest(abs)
	if err != nil {
		return nil, err
	}
	if digest != b.SnapshotSHA256 {
		return nil, ErrDigest
	}
	db, err := sql.Open("sqlite3", dsn(abs, "mode=ro&immutable=1&_query_only=1"))
	if err != nil {
		return nil, ErrSource
	}
	db.SetMaxOpenConns(1)
	return &frozen{db: db, path: abs, info: info}, nil
}
func (f *frozen) unchanged() error {
	i, e := os.Lstat(f.path)
	if e != nil || !os.SameFile(i, f.info) || i.Mode() != f.info.Mode() || i.Size() != f.info.Size() || !i.ModTime().Equal(f.info.ModTime()) {
		return ErrSource
	}
	return nil
}
func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Required columns fail closed on missing essential history domains. Additional
// columns/tables are archived too; no allow-list silently drops future evidence.
var required = map[string]string{
	"usage_events":                     "id event_hash timestamp_ms api_key_hash account_snapshot auth_account_id_snapshot auth_project_id_snapshot input_tokens output_tokens cached_tokens cache_creation_tokens raw_json",
	"usage_event_identity_ledger":      "event_hash",
	"settings":                         "key value updated_at_ms",
	"api_key_aliases":                  "api_key_hash alias updated_at_ms",
	"api_key_alias_history":            "api_key_hash current_hash",
	"model_prices":                     "model prompt_per_1m completion_per_1m cache_read_per_1m cache_creation_per_1m raw_json",
	"model_price_context_tiers":        "model threshold_tokens",
	"model_price_service_tiers":        "model service_tier",
	"account_quota_observations":       "id observation_hash account_key observed_at_ms",
	"account_quota_windows":            "id account_key provider_window_id",
	"account_quota_window_activations": "window_id generation activated_at_ms deactivated_at_ms",
	"account_quota_cycles":             "activation_id provider_cycle_key actual_start_ms actual_end_ms scheduled_end_ms",
	"account_quota_snapshots":          "observation_id logical_window_id activation_id cycle_id observed_at_ms",
	"analytics_state":                  "name value",
	"analytics_accounts":               "id provider label auth_index auth_file last_seen_ms",
	"analytics_events":                 "event_hash event_id account started_ms completed_ms payload",
	"analytics_event_keys":             "event_hash api_key_hash",
	"analytics_prices":                 "id model captured_at_ms payload",
	"analytics_observations":           "id account observed_ms window_id payload",
	"analytics_poll_runs":              "id account observed_ms source status",
	"analytics_normalization_rules":    "id captured_ms payload",
	"analytics_profile_revisions":      "revision created_ms action payload",
	"analytics_credential_inventory":   "account checked_ms status",
	"analytics_forecasts":              "id account window_id issued_ms reset_ms payload",
	"analytics_forecast_results":       "id forecast_id settled_ms state payload",
	"analytics_forecast_work":          "account generation next_ms after_forecast waiting",
}

//go:embed cpamp-schema.sql
var cpampSchema string

type columnShape struct {
	Type                        string
	NotNull, PrimaryKey, Hidden int
}

func columns(ctx context.Context, db *sql.DB, name string) (map[string]columnShape, error) {
	rows, e := db.QueryContext(ctx, "PRAGMA table_xinfo("+quote(name)+")")
	if e != nil {
		return nil, ErrSource
	}
	defer rows.Close()
	out := map[string]columnShape{}
	for rows.Next() {
		var cid int
		var n string
		var c columnShape
		var def any
		if rows.Scan(&cid, &n, &c.Type, &c.NotNull, &def, &c.PrimaryKey, &c.Hidden) != nil {
			return nil, ErrSource
		}
		c.Type = strings.ToUpper(c.Type)
		out[n] = c
	}
	if rows.Err() != nil {
		return nil, ErrSource
	}
	return out, nil
}
func checkSchema(ctx context.Context, db *sql.DB, b Boundary) error {
	contract, e := sql.Open("sqlite3", ":memory:")
	if e != nil {
		return ErrStorage
	}
	defer contract.Close()
	contract.SetMaxOpenConns(1)
	if _, e = contract.ExecContext(ctx, cpampSchema); e != nil {
		return ErrStorage
	}
	names := []string{}
	for n := range required {
		names = append(names, n)
	}
	if b.Schema == "cpamp-usage-v3" {
		names = append(names, "analytics_historical_valuations", "analytics_token_evidence")
	}
	for _, n := range names {
		want, e := columns(ctx, contract, n)
		if e != nil {
			return e
		}
		got, e := columns(ctx, db, n)
		if e != nil {
			return e
		}
		for col, shape := range want {
			if got[col] != shape {
				return ErrSource
			}
		}
	}
	return nil
}

func inspect(ctx context.Context, f *frozen, b Boundary) ([]schemaObject, []tableSpec, error) {
	var encoding string
	if f.db.QueryRowContext(ctx, `PRAGMA encoding`).Scan(&encoding) != nil || encoding != "UTF-8" {
		return nil, nil, ErrSource
	}
	if e := checkSchema(ctx, f.db, b); e != nil {
		return nil, nil, e
	}
	var integrity string
	if f.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity) != nil || integrity != "ok" {
		return nil, nil, ErrSource
	}
	rows, err := f.db.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`)
	if err != nil {
		return nil, nil, ErrSource
	}
	objects := []schemaObject{}
	for rows.Next() {
		var o schemaObject
		if rows.Scan(&o.Kind, &o.Name, &o.Table, &o.SQL) != nil {
			rows.Close()
			return nil, nil, ErrSource
		}
		objects = append(objects, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, ErrSource
	}
	tables := []tableSpec{}
	found := map[string]map[string]bool{}
	for _, o := range objects {
		if o.Kind != "table" {
			continue
		}
		t := tableSpec{Name: o.Name}
		ddl := strings.ToLower(o.SQL.String)
		if strings.Contains(ddl, "create virtual table") {
			// FTS5 physical backing tables are archived below, including their
			// WITHOUT ROWID indexes. Never execute source SQL on the target.
			if !strings.Contains(ddl, "using fts5") {
				return nil, nil, ErrSource
			}
			t.Virtual = true
			tables = append(tables, t)
			continue
		}
		r, e := f.db.QueryContext(ctx, "PRAGMA table_xinfo("+quote(o.Name)+")")
		if e != nil {
			return nil, nil, ErrSource
		}
		cols := map[string]bool{}
		pk := map[int]string{}
		for r.Next() {
			var cid, nn, key, hidden int
			var name, typ string
			var def any
			if r.Scan(&cid, &name, &typ, &nn, &def, &key, &hidden) != nil || hidden != 0 {
				r.Close()
				return nil, nil, ErrSource
			}
			t.Columns = append(t.Columns, name)
			cols[name] = true
			if key > 0 {
				pk[key] = name
			}
		}
		e = r.Err()
		r.Close()
		if e != nil || len(t.Columns) == 0 {
			return nil, nil, ErrSource
		}
		found[t.Name] = cols
		if strings.Contains(ddl, "without rowid") {
			for n := 1; n <= len(pk); n++ {
				t.Keys = append(t.Keys, pk[n])
			}
		} else {
			for _, key := range []string{"_rowid_", "rowid", "oid"} {
				shadowed := false
				for name := range cols {
					if strings.EqualFold(name, key) {
						shadowed = true
					}
				}
				if !shadowed {
					t.Keys = []string{key}
					break
				}
			}
		}
		if len(t.Keys) == 0 {
			return nil, nil, ErrSource
		}
		tables = append(tables, t)
	}
	for name, cols := range required {
		for _, c := range strings.Fields(cols) {
			if !found[name][c] {
				return nil, nil, ErrSource
			}
		}
	}
	for name, cols := range map[string]string{"analytics_historical_valuations": "id payload", "analytics_token_evidence": "revision event_hash source_id payload"} {
		if b.Schema == "cpamp-usage-v3" || found[name] != nil {
			for _, c := range strings.Fields(cols) {
				if !found[name][c] {
					return nil, nil, ErrSource
				}
			}
		}
	}
	if b.Schema == "cpamp-usage-v2" && (found["analytics_historical_valuations"] != nil || found["analytics_token_evidence"] != nil) {
		return nil, nil, ErrSource
	}
	// Preserve exact supplied checkpoints, but reject impossible source bounds.
	var events, observations int64
	if f.db.QueryRowContext(ctx, `SELECT coalesce((SELECT value FROM analytics_state WHERE name='events'),0),coalesce((SELECT max(rowid) FROM analytics_observations),0)`).Scan(&events, &observations) != nil || b.EventsThrough > events || b.ObservationsThrough > observations {
		return nil, nil, ErrSource
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	return objects, tables, nil
}

type archiveRow struct{ Key, Cells []Cell }

func readBatch(ctx context.Context, f *frozen, t tableSpec, cursor []Cell, limit int) ([]archiveRow, error) {
	cols := append(append([]string{}, t.Keys...), t.Columns...)
	selects := []string{}
	for _, c := range cols {
		q := quote(c)
		selects = append(selects, "typeof("+q+")", "CASE typeof("+q+") WHEN 'text' THEN CAST("+q+" AS BLOB) ELSE "+q+" END")
	}
	keys := []string{}
	for _, k := range t.Keys {
		keys = append(keys, quote(k))
	}
	query := "SELECT " + strings.Join(selects, ",") + " FROM " + quote(t.Name)
	args := []any{}
	if len(cursor) > 0 {
		if len(cursor) != len(keys) {
			return nil, ErrStorage
		}
		placeholders := []string{}
		for _, c := range cursor {
			v, e := c.value()
			if e != nil {
				return nil, ErrStorage
			}
			args = append(args, v)
			placeholders = append(placeholders, "?")
		}
		if len(keys) == 1 {
			query += " WHERE " + keys[0] + ">?"
		} else {
			query += " WHERE (" + strings.Join(keys, ",") + ")>(" + strings.Join(placeholders, ",") + ")"
		}
	}
	query += " ORDER BY " + strings.Join(keys, ",") + " LIMIT ?"
	args = append(args, limit)
	rows, e := f.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, ErrSource
	}
	defer rows.Close()
	result := []archiveRow{}
	bytes := 0
	for rows.Next() {
		values := make([]any, len(selects))
		ptr := make([]any, len(values))
		for i := range ptr {
			ptr[i] = &values[i]
		}
		if rows.Scan(ptr...) != nil {
			return nil, ErrSource
		}
		cells := []Cell{}
		for i := 0; i < len(values); i += 2 {
			kind, ok := values[i].(string)
			if !ok {
				return nil, ErrSource
			}
			c, e := cell(kind, values[i+1])
			if e != nil {
				return nil, e
			}
			cells = append(cells, c)
		}
		r := archiveRow{Key: cells[:len(keys)], Cells: cells[len(keys):]}
		size := len(encode(r))
		if size > 16<<20 {
			return nil, ErrSource
		}
		bytes += size
		result = append(result, r)
		if bytes >= 4<<20 {
			break
		}
	}
	if rows.Err() != nil {
		return nil, ErrSource
	}
	return result, nil
}

func rowMap(t tableSpec, r archiveRow) map[string]Cell {
	m := map[string]Cell{}
	for i, c := range t.Columns {
		m[c] = r.Cells[i]
	}
	return m
}
func chainHash(prior string, r archiveRow) string {
	return hash([]byte(fmt.Sprintf("%s\n%s", prior, encode(r))))
}
