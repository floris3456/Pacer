package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()
var keyA = hash([]byte("synthetic-current"))
var keyB = hash([]byte("synthetic-retired"))
var keyC = hash([]byte("synthetic-other"))

const secret = "sk-synthetic-secret-must-not-appear"

func mustExec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, e := db.Exec(q, args...); e != nil {
		t.Fatal(e)
	}
}

// Fixtures use the copied actual CPAMP schema, not importer-shaped toy tables.
// Additional source tables exercise forward archival preservation separately.
func fixture(t *testing.T, modify func(*sql.DB)) ImportOptions {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frozen.sqlite")
	db, e := sql.Open("sqlite3", path)
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, cpampSchema)
	mustExec(t, db, `PRAGMA user_version=42;PRAGMA application_id=43`)
	mustExec(t, db, `INSERT INTO settings VALUES('management-key',?,1)`, secret)
	mustExec(t, db, `INSERT INTO api_key_aliases VALUES(?,?,1),(?,?,2)`, keyA, "Work alias", keyC, "Other")
	mustExec(t, db, `INSERT INTO api_key_alias_history VALUES(?,?)`, keyB, keyA)
	mustExec(t, db, `INSERT INTO usage_events(id,event_hash,timestamp_ms,timestamp,model,api_key_hash,raw_json,fail_body,created_at_ms) VALUES(1,'event-1',10,'timestamp','model',?,?,?,11)`, keyB, `{"api_key":"`+secret+`"}`, secret)
	mustExec(t, db, `INSERT INTO analytics_events VALUES('event-1',1,'account-private',10,11,?)`, `{"api_key_hash":"`+keyB+`","source_tokens":{"write_5m":250000,"write_1h":750000}}`)
	mustExec(t, db, `INSERT INTO analytics_event_keys VALUES('event-1',?)`, keyB)
	mustExec(t, db, `INSERT INTO analytics_state VALUES('events',1),('observations',1),('historical_valuation',1)`)
	mustExec(t, db, `INSERT INTO analytics_accounts VALUES('account-private','claude','Account display','auth-index',?,10,0,'')`, secret)
	mustExec(t, db, `INSERT INTO analytics_observations VALUES('obs-1','account-private',20,'window-1',?)`, `{"closed_cycle":{"end_ms":1790812800000,"status":"closed"},"source":"fixture"}`)
	mustExec(t, db, `INSERT INTO analytics_profile_revisions VALUES(0,0,'initial','{"revision":0,"profiles":[],"bindings":[]}')`)
	mustExec(t, db, `INSERT INTO analytics_profile_revisions VALUES(7,22,'edit',?)`, `{"revision":7,"profiles":[{"id":"profile-1","name":"Subscription","provider":"claude","memberships":[{"account":"account-private","start_ms":10,"end_ms":20}]},{"id":"selection-1","name":"Selection","selection":true,"accounts":["account-private"]}],"bindings":[],"plans":[{"preserve":"unknown-fields"}],"future":{"a":1}}`)
	mustExec(t, db, `INSERT INTO analytics_prices VALUES('price-book','model',23,?)`, `{"policy":"profile-api-value-v3","events_through":1,"observations_through":1,"forecast_through":4,"historical":{"source_checkpoint":"frozen-cp","h_ms":1790899200000},"missing_price":true}`)
	mustExec(t, db, `INSERT INTO analytics_normalization_rules VALUES('pin',24,?)`, `{"source_revision":7,"book_id":"price-book","allocation_bounds":[1,2],"historical_policy":"v3"}`)
	mustExec(t, db, `INSERT INTO analytics_historical_valuations VALUES('cohort',?)`, `{"source_checkpoint":"frozen-cp","h_ms":1790899200000,"cutoff_ms":1790726400000,"events_through":1,"observations_through":1,"windows":{"window-1":{"source":"fixture"}}}`)
	mustExec(t, db, `INSERT INTO analytics_token_evidence(event_hash,source_id,payload) VALUES('event-1','provider',?)`, `{"write_5m":250000,"write_1h":750000}`)
	mustExec(t, db, `INSERT INTO usage_event_identity_ledger(event_hash,raw_event_id,timestamp_ms,bucket_ms,first_seen_at_ms,updated_at_ms) VALUES('event-1',1,10,0,10,11)`)
	mustExec(t, db, `INSERT INTO model_prices(model,prompt_per_1m,completion_per_1m,cache_per_1m,cache_read_per_1m,cache_creation_per_1m,raw_json,updated_at_ms) VALUES('model',3,15,.3,.3,3.75,'{"retained":true}',23);
INSERT INTO model_price_context_tiers(model,threshold_tokens,cache_creation_per_1m,cache_creation_configured) VALUES('model',200000,7.5,1);
INSERT INTO model_price_service_tiers(model,mode,service_tier,cache_creation_per_1m,cache_creation_configured) VALUES('model','absolute','priority',6,1);
INSERT INTO analytics_forecasts VALUES('forecast','account-private','window-1',19,30,'{"pin":"forecast"}');
INSERT INTO analytics_forecast_results VALUES('settled','forecast',31,'settled','{"pin":"settled"}');
INSERT INTO account_quota_observations(id,observation_hash,account_key,provider,source,inventory_scope_key,inventory_mode,observed_at_ms,created_at_ms) VALUES(1,'obs-hash','account-private','claude','header','all','full',20,21);
INSERT INTO account_quota_windows(id,account_key,provider,provider_window_id,window_kind,window_mode,model_scope_kind,scope_fingerprint,inventory_scope_key,availability,first_seen_at_ms,last_seen_at_ms,created_at_ms,updated_at_ms) VALUES(1,'account-private','claude','window-1','week','rolling','all','scope','all','available',1,20,1,21);
INSERT INTO account_quota_window_activations(id,window_id,generation,status,activated_at_ms,deactivated_at_ms,activation_accuracy,created_at_ms,updated_at_ms) VALUES(1,1,1,'closed',1,20,'exact',1,21);
INSERT INTO account_quota_cycles(id,activation_id,provider_cycle_key,state,scheduled_end_ms,actual_start_ms,actual_end_ms,boundary_accuracy,end_reason,created_at_ms,updated_at_ms) VALUES(1,1,'cycle-1','closed',30,1,20,'exact','early-reset',1,21);
INSERT INTO account_quota_snapshots(id,observation_id,logical_window_id,activation_id,cycle_id,account_key,provider,provider_window_id,window_kind,window_mode,model_scope_kind,source,observed_at_ms,boundary_accuracy,cycle_start_ms,cycle_end_ms,used_percent,created_at_ms) VALUES(1,1,1,1,1,'account-private','claude','window-1','week','rolling','all','header',20,'exact',1,20,50,21);`)
	mustExec(t, db, `CREATE TABLE extra_history(k TEXT,n INTEGER,b BLOB,r REAL,t TEXT,PRIMARY KEY(k,n)) WITHOUT ROWID;
INSERT INTO extra_history VALUES('a',-2,x'00ff',1.25,CAST(x'ff00' AS TEXT)),('a',3,x'',NULL,''),('b',1,NULL,-1.5,'z');`)
	mustExec(t, db, `CREATE TABLE extra_rowids(id INTEGER PRIMARY KEY, v TEXT);INSERT INTO extra_rowids VALUES(-9,'negative'),(0,'zero'),(9,'positive');
CREATE TABLE rowid_shadow("_RoWiD_" TEXT,v TEXT);INSERT INTO rowid_shadow VALUES('same','first'),('same','second');`)
	if modify != nil {
		modify(db)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(path, 0400); e != nil {
		t.Fatal(e)
	}
	digest, _, e := frozenDigest(path)
	if e != nil {
		t.Fatal(e)
	}
	return ImportOptions{SourcePath: path, Boundary: Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: digest, LineageSHA256: hash([]byte("fixture-installation")), CheckpointSHA256: hash([]byte("fixture-cutover")), HMS: 1790899200000, EventsThrough: 1, ObservationsThrough: 1}, BatchSize: 2}
}
func store(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFrozenImportRepeatRestartAndExactArchive(t *testing.T) {
	o := fixture(t, nil)
	s := store(t)
	o.MaxBatches = 4
	p, e := s.Import(ctx, o)
	if e != nil || p.Complete {
		t.Fatalf("staging: %+v %v", p, e)
	}
	if _, e = s.Keys(ctx); !errors.Is(e, ErrPending) {
		t.Fatalf("partial published: %v", e)
	}
	if _, e = s.Profiles(ctx, -1); !errors.Is(e, ErrPending) {
		t.Fatal(e)
	}
	path := s.path
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o.MaxBatches = 0
	p, e = s.Import(ctx, o)
	if e != nil || !p.Complete {
		t.Fatalf("resume: %+v %v", p, e)
	}
	before := encode(p)
	p, e = s.Import(ctx, o)
	if e != nil || !bytes.Equal(before, encode(p)) {
		t.Fatal("repeat changed manifest", e)
	}
	f, e := openFrozen(o.SourcePath, o.Boundary)
	if e != nil {
		t.Fatal(e)
	}
	defer f.db.Close()
	_, tables, e := inspect(ctx, f, o.Boundary)
	if e != nil {
		t.Fatal(e)
	}
	for _, table := range tables {
		if table.Virtual {
			continue
		}
		source, e := readBatch(ctx, f, table, nil, 1000)
		if e != nil {
			t.Fatal(e)
		}
		rows, e := s.db.Query(`SELECT key,cells FROM history_rows WHERE tbl=? ORDER BY ordinal`, table.Name)
		if e != nil {
			t.Fatal(e)
		}
		i := 0
		chain := ""
		for rows.Next() {
			var k, c []byte
			if rows.Scan(&k, &c) != nil {
				t.Fatal("scan")
			}
			if i >= len(source) || !bytes.Equal(k, encode(source[i].Key)) || !bytes.Equal(c, encode(source[i].Cells)) {
				t.Fatalf("source mismatch: %s row %d", table.Name, i)
			}
			chain = chainHash(chain, source[i])
			i++
		}
		rows.Close()
		if i != len(source) {
			t.Fatal("missing rows", table.Name)
		}
		var got string
		if s.db.QueryRow(`SELECT chain FROM history_tables WHERE name=?`, table.Name).Scan(&got) != nil || got != chain {
			t.Fatal("chain mismatch")
		}
	}
	// Independent expected typed-cell assertions (not just the reader's encoding).
	var raw []byte
	if s.db.QueryRow(`SELECT cells FROM history_rows WHERE tbl='extra_history' AND ordinal=1`).Scan(&raw) != nil {
		t.Fatal("missing extra")
	}
	var cells []Cell
	json.Unmarshal(raw, &cells)
	if cells[1] != (Cell{Type: "integer", Data: "-2"}) || cells[2] != (Cell{Type: "blob", Data: "AP8="}) || cells[4] != (Cell{Type: "text", Data: "/wA="}) {
		t.Fatalf("lossy cells: %+v", cells)
	}
	digest, _, e := frozenDigest(o.SourcePath)
	if e != nil || digest != o.Boundary.SnapshotSHA256 {
		t.Fatal("source modified")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e := os.Stat(o.SourcePath + suffix); !os.IsNotExist(e) {
			t.Fatal("source side effect")
		}
	}
	if _, e = f.db.Exec(`DELETE FROM usage_events`); e == nil {
		t.Fatal("source writable")
	}
}

func TestKeysProfilesEditsAndSecretRedaction(t *testing.T) {
	o := fixture(t, func(db *sql.DB) {
		mustExec(t, db, `UPDATE api_key_aliases SET alias=? WHERE api_key_hash=?`, secret, keyC)
	})
	s := store(t)
	if _, e := s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	c, e := s.Keys(ctx)
	if e != nil || len(c.Keys) != 2 {
		t.Fatal(c, e)
	}
	var current Key
	for _, k := range c.Keys {
		if k.CurrentHash == keyA {
			current = k
		} else if k.Alias != "[redacted]" {
			t.Fatal("unsafe alias")
		}
	}
	if current.Alias != "Work alias" || !reflect.DeepEqual(current.Lineage, sorted(keyA, keyB)) {
		t.Fatal(current)
	}
	profiles, e := s.Profiles(ctx, 7)
	if e != nil || profiles.Revision != 7 || len(profiles.Profiles) != 2 {
		t.Fatal(profiles, e)
	}
	if got := profiles.Profiles[0].Memberships[0]; got.Start != 10 || got.End != 20 || got.AccountReference != "account:"+hash([]byte("account-private")) {
		t.Fatal(got)
	}
	if !profiles.Profiles[1].Selection || profiles.Profiles[1].Memberships[0].End != 0 {
		t.Fatal("selection lost")
	}
	initial, e := s.Profiles(ctx, 0)
	if e != nil || len(initial.Profiles) != 0 {
		t.Fatal(initial, e)
	}
	c, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "alias", Reference: current.Reference, Name: "Renamed"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Edit(ctx, Edit{Revision: c.Revision - 1, Action: "alias", Reference: current.Reference, Name: "Stale"}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	ref := "group:" + hash([]byte("group-1"))
	c, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "put-group", Reference: ref, Name: "Display group", Members: []string{current.Reference}})
	if e != nil {
		t.Fatal(e)
	}
	newHash := hash([]byte("new-key"))
	c, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "rotate-key", Reference: current.Reference, Hash: newHash})
	if e != nil {
		t.Fatal(e)
	}
	if c.Groups[0].Members[0] != current.Reference {
		t.Fatal("rotation detached group")
	}
	for _, k := range c.Keys {
		if k.Reference == current.Reference && !reflect.DeepEqual(k.Lineage, sorted(keyA, keyB, newHash)) {
			t.Fatal("lost rotation", k)
		}
	}
	if _, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "rotate-key", Reference: current.Reference, Hash: keyC}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "alias", Reference: current.Reference, Name: secret}); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	after, e := s.Profiles(ctx, 7)
	if e != nil || !reflect.DeepEqual(profiles, after) {
		t.Fatal("native edits mutated imported profiles")
	}
	if _, e = s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	repeated, e := s.Keys(ctx)
	if e != nil || !reflect.DeepEqual(c, repeated) {
		t.Fatal("repeat erased edits")
	}
	path := s.path
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	reopened, e := s.Keys(ctx)
	if e != nil || !reflect.DeepEqual(c, reopened) {
		t.Fatal("restart lost metadata")
	}
	status, e := s.Status(ctx)
	if e != nil {
		t.Fatal(e)
	}
	public := string(encode([]any{reopened, after, status}))
	for _, bad := range []string{secret, "account-private", "auth-index", "management-key", o.SourcePath, "raw_json"} {
		if strings.Contains(public, bad) {
			t.Fatalf("public leak %q", bad)
		}
	}
	// The private archive keeps the original alias byte-exact despite view redaction.
	var preserved int
	secretCell, _ := cell("text", []byte(secret))
	if s.db.QueryRow(`SELECT count(*) FROM history_rows WHERE tbl='api_key_aliases' AND instr(cells,?)>0`, secretCell.Data).Scan(&preserved) != nil || preserved != 1 {
		t.Fatal("missing exact archived alias")
	}
}
func sorted(v ...string) []string { sort.Strings(v); return v }

func TestBoundaryFailuresAreClosed(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*sql.DB)
		change func(*ImportOptions)
		want   error
	}{
		{"missing-table", func(db *sql.DB) { mustExec(t, db, `DROP TABLE analytics_prices`) }, nil, ErrSource},
		{"wrong-schema", func(db *sql.DB) {
			mustExec(t, db, `ALTER TABLE api_key_aliases RENAME TO old_aliases;CREATE TABLE api_key_aliases(api_key_hash INTEGER PRIMARY KEY,alias TEXT NOT NULL,updated_at_ms INTEGER NOT NULL)`)
		}, nil, ErrSource},
		{"conflicting-profile-revision", func(db *sql.DB) {
			mustExec(t, db, `UPDATE analytics_profile_revisions SET payload='{"revision":8}' WHERE revision=7`)
		}, nil, ErrConflict},
		{"lineage-cycle", func(db *sql.DB) { mustExec(t, db, `INSERT INTO api_key_alias_history VALUES(?,?)`, keyA, keyB) }, nil, ErrSource},
		{"retired-alias", func(db *sql.DB) { mustExec(t, db, `INSERT INTO api_key_aliases VALUES(?,?,1)`, keyB, "Retired") }, nil, ErrSource},
		{"raw-key", func(db *sql.DB) { mustExec(t, db, `UPDATE usage_events SET api_key_hash=?`, secret) }, nil, ErrSource},
		{"digest", nil, func(o *ImportOptions) { o.Boundary.SnapshotSHA256 = hash([]byte("wrong")) }, ErrDigest},
		{"future-checkpoint", nil, func(o *ImportOptions) { o.Boundary.EventsThrough = 2 }, ErrSource},
		{"v3-as-v2", nil, func(o *ImportOptions) { o.Boundary.Schema = "cpamp-usage-v2" }, ErrSource},
		{"writable", nil, func(o *ImportOptions) { os.Chmod(o.SourcePath, 0600) }, ErrSource},
		{"sidecar", nil, func(o *ImportOptions) { os.WriteFile(o.SourcePath+"-wal", []byte("not a frozen backup"), 0600) }, ErrSource},
		{"symlink", nil, func(o *ImportOptions) {
			link := o.SourcePath + ".link"
			os.Symlink(o.SourcePath, link)
			o.SourcePath = link
		}, ErrSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := fixture(t, tt.edit)
			if tt.change != nil {
				tt.change(&o)
			}
			s := store(t)
			_, e := s.Import(ctx, o)
			if !errors.Is(e, tt.want) {
				t.Fatalf("got %v want %v", e, tt.want)
			}
			if e != nil && strings.Contains(e.Error(), secret) {
				t.Fatal("error leak")
			}
			p, e := s.Status(ctx)
			if e == nil && p.Complete {
				t.Fatal("failed import published")
			}
		})
	}
}

func TestImmutableBoundaryAndForeignDestination(t *testing.T) {
	o := fixture(t, nil)
	s := store(t)
	o.MaxBatches = 1
	if _, e := s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"lineage", "checkpoint", "H", "digest"} {
		bad := o
		switch field {
		case "lineage":
			bad.Boundary.LineageSHA256 = hash([]byte("another-installation"))
		case "checkpoint":
			bad.Boundary.CheckpointSHA256 = hash([]byte("another-cutover"))
		case "H":
			bad.Boundary.HMS++
		case "digest":
			bad.Boundary.SnapshotSHA256 = hash([]byte("wrong"))
		}
		_, e := s.Import(ctx, bad)
		if !errors.Is(e, ErrConflict) && !errors.Is(e, ErrDigest) {
			t.Fatalf("%s %v", field, e)
		}
	}
	os.Chmod(o.SourcePath, 0600)
	before, e := os.ReadFile(o.SourcePath)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Open(o.SourcePath); !errors.Is(e, ErrConflict) {
		t.Fatal("opened foreign DB", e)
	}
	after, _ := os.ReadFile(o.SourcePath)
	if !bytes.Equal(before, after) {
		t.Fatal("modified foreign db")
	}
	// A different valid frozen snapshot cannot substitute for an interrupted one.
	db, e := sql.Open("sqlite3", o.SourcePath)
	if e != nil {
		t.Fatal(e)
	}
	mustExec(t, db, `UPDATE settings SET value='changed'`)
	db.Close()
	os.Chmod(o.SourcePath, 0400)
	o.Boundary.SnapshotSHA256, _, e = frozenDigest(o.SourcePath)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Import(ctx, o); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestConcurrentNativeRevisionAndFreshKeys(t *testing.T) {
	s := store(t)
	c, e := s.Edit(ctx, Edit{Action: "add-key", Hash: keyA, Name: "A"})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, label := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Edit(ctx, Edit{Revision: c.Revision, Action: "alias", Reference: "key:" + keyA, Name: label})
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	ok, conflicts := 0, 0
	for e := range errs {
		if e == nil {
			ok++
		} else if errors.Is(e, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatal(ok, conflicts)
	}
	o := fixture(t, nil)
	if _, e = s.Import(ctx, o); !errors.Is(e, ErrConflict) {
		t.Fatal("import over native edits", e)
	}
}

func TestBatchRollbackThenRestart(t *testing.T) {
	o := fixture(t, nil)
	s := store(t)
	// Deterministic mid-batch storage failure after INSERT but before progress.
	mustExec(t, s.db, `CREATE TRIGGER fail_batch BEFORE UPDATE ON history_tables WHEN new.name='analytics_events' BEGIN SELECT RAISE(ABORT,'synthetic interruption');END`)
	if _, e := s.Import(ctx, o); !errors.Is(e, ErrStorage) {
		t.Fatal(e)
	}
	var n int
	if s.db.QueryRow(`SELECT count(*) FROM history_rows WHERE tbl='analytics_events'`).Scan(&n) != nil || n != 0 {
		t.Fatal("partial transaction leaked")
	}
	mustExec(t, s.db, `DROP TRIGGER fail_batch`)
	path := s.path
	s.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := s.Import(ctx, o)
	if e != nil || !p.Complete {
		t.Fatal(e)
	}
	if s.db.QueryRow(`SELECT count(*) FROM history_rows WHERE tbl='analytics_events'`).Scan(&n) != nil || n != 1 {
		t.Fatal("resume duplicated row")
	}
}

func TestLargeHistoryBoundedResume(t *testing.T) {
	if testing.Short() {
		t.Skip("100k offline archive")
	}
	o := fixture(t, func(db *sql.DB) {
		tx, e := db.Begin()
		if e != nil {
			t.Fatal(e)
		}
		stmt, e := tx.Prepare(`INSERT INTO analytics_events VALUES(?,?,'account-private',10,11,'{}')`)
		if e != nil {
			t.Fatal(e)
		}
		for i := 2; i <= 100001; i++ {
			if _, e = stmt.Exec(fmt.Sprintf("event-%d", i), i); e != nil {
				t.Fatal(e)
			}
		}
		stmt.Close()
		if e = tx.Commit(); e != nil {
			t.Fatal(e)
		}
	})
	s := store(t)
	o.BatchSize = 333
	o.MaxBatches = 20
	p, e := s.Import(ctx, o)
	if e != nil || p.Complete {
		t.Fatal(p, e)
	}
	var partial int64
	if s.db.QueryRow(`SELECT rows FROM history_tables WHERE name='analytics_events'`).Scan(&partial) != nil || partial == 0 || partial > 6660 {
		t.Fatal("not bounded", partial)
	}
	path := s.path
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o.MaxBatches = 0
	p, e = s.Import(ctx, o)
	if e != nil || !p.Complete {
		t.Fatal(e)
	}
	var count, unique int
	if s.db.QueryRow(`SELECT count(*),count(DISTINCT key) FROM history_rows WHERE tbl='analytics_events'`).Scan(&count, &unique) != nil || count != 100001 || unique != 100001 {
		t.Fatal(count, unique)
	}
}

func TestV2AndLegacyMemberships(t *testing.T) {
	o := fixture(t, func(db *sql.DB) {
		mustExec(t, db, `DROP TABLE analytics_historical_valuations;DROP TABLE analytics_token_evidence;UPDATE analytics_profile_revisions SET payload='{"revision":7,"profiles":[{"id":"legacy","name":"Legacy"}],"bindings":[{"profile":"legacy","account":"account-private","start_ms":5,"end_ms":9}]}' WHERE revision=7`)
	})
	o.Boundary.Schema = "cpamp-usage-v2"
	s := store(t)
	if _, e := s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	p, e := s.Profiles(ctx, -1)
	if e != nil || p.Profiles[0].Memberships[0].Start != 5 || p.Profiles[0].Memberships[0].End != 9 {
		t.Fatal(p, e)
	}
}

func TestFTS5BackingTables(t *testing.T) {
	probe, e := sql.Open("sqlite3", ":memory:")
	if e != nil {
		t.Fatal(e)
	}
	_, e = probe.Exec(`CREATE VIRTUAL TABLE probe USING fts5(text)`)
	probe.Close()
	if e != nil {
		t.Skip("requires -tags sqlite_fts5")
	}
	o := fixture(t, func(db *sql.DB) {
		mustExec(t, db, `CREATE VIRTUAL TABLE search_history USING fts5(text, tokenize='trigram');INSERT INTO search_history VALUES('preserved index evidence')`)
	})
	s := store(t)
	if _, e := s.Import(ctx, o); e != nil {
		t.Fatal(e)
	}
	var spec []byte
	if s.db.QueryRow(`SELECT spec FROM history_tables WHERE name='search_history'`).Scan(&spec) != nil || !strings.Contains(string(spec), `"Virtual":true`) {
		t.Fatal("missing virtual schema")
	}
	for _, table := range []string{"search_history_data", "search_history_idx", "search_history_config", "search_history_content", "search_history_docsize"} {
		var n int
		if s.db.QueryRow(`SELECT rows FROM history_tables WHERE name=?`, table).Scan(&n) != nil || n == 0 {
			t.Fatal("missing physical FTS", table)
		}
	}
}

func TestReportPinsAndSourceVersions(t *testing.T) {
	o := fixture(t, nil)
	pins := []byte(`{"window_id":"window-1","cursor":"pinned-proof","revision":7,"historical":{"h_ms":1790899200000},"private_fixture":"` + secret + `"}`)
	o.ReportPinsPath = filepath.Join(t.TempDir(), "report-pins.json")
	if e := os.WriteFile(o.ReportPinsPath, pins, 0400); e != nil {
		t.Fatal(e)
	}
	o.Boundary.ReportPinsSHA256 = hash(pins)
	s := store(t)
	p, e := s.Import(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(encode(p)), secret) {
		t.Fatal("artifact exposed")
	}
	var raw []byte
	if s.db.QueryRow(`SELECT payload FROM history_artifacts WHERE name='report-pins'`).Scan(&raw) != nil || !bytes.Equal(raw, pins) {
		t.Fatal("lost report pins")
	}
	for name, want := range map[string]string{"user_version": "42", "application_id": "43", "encoding": "UTF-8"} {
		var got string
		if s.db.QueryRow(`SELECT value FROM history_source WHERE name=?`, name).Scan(&got) != nil || got != want {
			t.Fatal("source version lost", name)
		}
	}
	bad := o
	bad.Boundary.ReportPinsSHA256 = hash([]byte("changed"))
	if _, e = s.Import(ctx, bad); !errors.Is(e, ErrDigest) {
		t.Fatal(e)
	}
	bad = o
	bad.ReportPinsPath = ""
	if _, e = s.Import(ctx, bad); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	bad = o
	bad.ReportPinsPath = ""
	bad.Boundary.ReportPinsSHA256 = ""
	if _, e = s.Import(ctx, bad); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestConcurrentImportProcesses(t *testing.T) {
	o := fixture(t, nil)
	s := store(t)
	other, e := Open(s.path)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, st := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := st.Import(ctx, o)
			if e == nil && !p.Complete {
				e = ErrPending
			}
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	var n int
	if s.db.QueryRow(`SELECT count(*) FROM history_metadata`).Scan(&n) != nil || n != 1 {
		t.Fatal("duplicate publication")
	}
}

func TestNativeGroupValidationAndDeletion(t *testing.T) {
	s := store(t)
	c, e := s.Edit(ctx, Edit{Action: "add-key", Hash: keyA, Name: "A"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "add-key", Hash: keyC, Name: " a "}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	c, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "add-key", Hash: keyC, Name: "C"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "alias", Reference: "key:" + keyC, Hash: keyA, Name: "A"}); !errors.Is(e, ErrConflict) {
		t.Fatal("alias collision bypass", e)
	}
	ref := "group:" + hash([]byte("display"))
	for _, members := range [][]string{{"key:" + keyB}, {"key:" + keyA, "key:" + keyA}} {
		if _, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "put-group", Reference: ref, Name: "Display", Members: members}); !errors.Is(e, ErrInput) {
			t.Fatal(e)
		}
	}
	c, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "put-group", Reference: ref, Name: "Display", Members: []string{"key:" + keyA, "key:" + keyC}})
	if e != nil {
		t.Fatal(e)
	}
	c, e = s.Edit(ctx, Edit{Revision: c.Revision, Action: "delete-group", Reference: ref})
	if e != nil || len(c.Groups) != 0 || len(c.Keys) != 2 {
		t.Fatal("delete changed keys", c, e)
	}
}
