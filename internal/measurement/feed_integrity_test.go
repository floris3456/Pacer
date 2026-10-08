package measurement

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func auditRejectFeedInsert(t *testing.T, db *sql.DB) func() {
	t.Helper()
	if db.Stats().MaxOpenConnections != 1 {
		t.Fatal("SQL fault fixture requires a one-connection pool")
	}
	change := func(fn func(*sqlite3.SQLiteConn) error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err = c.Raw(func(raw any) error { return fn(raw.(*sqlite3.SQLiteConn)) }); err != nil {
			t.Fatal(err)
		}
	}
	var original *sqlite3.SQLiteConn
	var denied atomic.Int64
	change(func(c *sqlite3.SQLiteConn) error {
		original = c
		c.RegisterAuthorizer(func(op int, table, column, database string) int {
			if op == sqlite3.SQLITE_INSERT && database == "main" && table == "measurement_report_feed" {
				denied.Add(1)
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	var once sync.Once
	restore := func() {
		t.Helper()
		once.Do(func() {
			change(func(c *sqlite3.SQLiteConn) error {
				if c != original {
					return errors.New("SQL fault fixture replaced its connection")
				}
				c.RegisterAuthorizer(nil)
				return nil
			})
			if denied.Load() == 0 {
				t.Error("SQL fault did not reach INSERT into measurement_report_feed")
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

// This intentionally models an older compatible writer: it knows the immutable
// revision schema, but executes none of the new application's feed guards.
func auditOldRevision(t *testing.T, db *sql.DB, r Record) {
	t.Helper()
	_, body, identity, err := canonicalRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO measurement_record_revisions VALUES(?,?,?,?,?)`, r.EventID, r.Revision, base.UnixNano(), body, []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO measurement_records VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.EventID, r.SourceID, r.AttemptID, r.Attribution.Provider, r.Attribution.AccountID, r.StartedAt.UnixNano(), r.EndedAt.UnixNano(), r.Revision, identity, base.UnixNano(), body, []byte{}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditFeedLostTrigger(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-bootstrap", true: "after-bootstrap"}[complete], func(t *testing.T) {
			s, db := fixture(t)
			put(t, s, record("z-existing", base, 1))
			if complete {
				if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err != nil || !done {
					t.Fatal(done, err)
				}
			}
			h, err := s.ReportFeedHead(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`DROP TRIGGER measurement_report_revision_insert`); err != nil {
				t.Fatal(err)
			}
			if _, err = s.PutRecord(ctx, record("a-missing", base, 1)); err == nil {
				t.Error("F3 expected lost-trigger write refusal; got successful usage ACK")
			}
			var count int
			if err = db.QueryRow(`SELECT count(*) FROM measurement_record_revisions WHERE event_id='a-missing'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Errorf("F3 expected rejected revision absent, got %d rows", count)
			}
			if h, err := s.ReportFeedHead(ctx); err == nil {
				t.Errorf("F3 expected unavailable head, got %+v", h)
			}
			if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err == nil || done {
				t.Errorf("F3 expected unavailable bootstrap/done=false, got done=%v err=%v", done, err)
			}
			if complete {
				if p, err := s.ReadReportFeed(ctx, h, 0, ReportBudget{}); err == nil {
					t.Errorf("F3 expected unavailable fixed-head read, got done=%v rows=%d", p.Done, len(p.Entries))
				}
			}
		})
	}
}

func TestAuditFeedRecreatedTriggerDoesNotEraseGap(t *testing.T) {
	s, db := fixture(t)
	put(t, s, record("z-existing", base, 1))
	if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err != nil || !done {
		t.Fatal(done, err)
	}
	h, err := s.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var definition string
	if err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='measurement_report_revision_insert'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER measurement_report_revision_insert`); err != nil {
		t.Fatal(err)
	}
	auditOldRevision(t, db, record("a-behind-cursor", base, 1))
	if _, err = db.Exec(definition); err != nil {
		t.Fatal(err)
	}
	if h, err := s.ReportFeedHead(ctx); err == nil {
		t.Errorf("F3 recreate: expected unavailable, got unchanged apparently complete head %+v", h)
	}
	if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err == nil || done {
		t.Errorf("F3 recreate: bootstrap trusted missed old-writer row: done=%v err=%v", done, err)
	}
	if p, err := s.ReadReportFeed(ctx, h, 0, ReportBudget{}); err == nil {
		t.Errorf("F3 recreate: expected refusal, read %d rows with done=%v (missing a-behind-cursor)", len(p.Entries), p.Done)
	}
	if _, err = s.PutRecord(ctx, record("new-after-gap", base, 1)); err == nil {
		t.Error("F3 recreate: new writer ACKed unresolved feed epoch")
	}
	if _, err = Open(ctx, db, Options{}); err == nil {
		t.Error("F3 recreate: reopen forgot durable integrity loss")
	}
	if r, err := s.Record(ctx, "a-behind-cursor", 1); err != nil || r.EventID == "" {
		t.Fatal("old writer's usage must remain retained", err)
	}
}

func TestAuditFeedSchemaChangeInsideWriteRollsBack(t *testing.T) {
	s, db := fixture(t)
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DROP TRIGGER measurement_report_revision_insert`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO measurement_record_revisions VALUES('inside-write',1,1,'{}',X'')`)
		return err
	})
	if err == nil {
		t.Error("F3 expected end-of-transaction integrity refusal, got commit")
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM measurement_record_revisions WHERE event_id='inside-write'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("F3 expected transaction rollback, got %d retained revision", n)
	}
}

func TestAuditFeedPhysicalInitializationCompatible(t *testing.T) {
	s, _ := fixture(t)
	if err := s.StartSourceConsumer(ctx, "audit-consumer", "audit-source", base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewPhysicalProof(ctx, "audit-consumer"); err != nil {
		t.Fatal("compiled additive physical initialization rejected", err)
	}
	put(t, s, record("after-physical-init", base, 1))
	if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err != nil || !done {
		t.Fatal(done, err)
	}
	h, err := s.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadReportFeed(ctx, h, 0, ReportBudget{})
	if err != nil || !page.Done || len(page.Entries) != 1 {
		t.Fatal(page, err)
	}
}

func TestAuditFeedOlderWriterStillJournaled(t *testing.T) {
	s, db := fixture(t)
	auditOldRevision(t, db, record("older-writer", base, 1))
	if done, err := s.BootstrapReportFeed(ctx, ReportBudget{}); err != nil || !done {
		t.Fatal(done, err)
	}
	h, err := s.ReportFeedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ReadReportFeed(ctx, h, 0, ReportBudget{})
	if err != nil || !page.Done || len(page.Entries) != 1 || page.Entries[0].EventID != "older-writer" {
		t.Fatal(page, err)
	}
	if _, err = Open(ctx, db, Options{}); err != nil {
		t.Fatal("unchanged schema must survive reopen", err)
	}
}

func TestAuditFeedUnknownSchemaChangesStayUnavailable(t *testing.T) {
	for _, mode := range []string{"unrelated-index", "unguarded-version"} {
		t.Run(mode, func(t *testing.T) {
			s, db := fixture(t)
			command := `CREATE INDEX audit_untrusted_index ON measurement_records(account_id)`
			if mode == "unguarded-version" {
				command = `UPDATE measurement_report_meta SET version=1`
			}
			if _, err := db.Exec(command); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReportFeedHead(ctx); !errors.Is(err, ErrReportFeedIntegrity) {
				t.Fatalf("expected integrity unavailable, got %v", err)
			}
			if _, err := Open(ctx, db, Options{}); !errors.Is(err, ErrReportFeedIntegrity) {
				t.Fatalf("reopen blessed unknown schema/version: %v", err)
			}
			if _, err := s.PutRecord(ctx, record("refused", base, 1)); !errors.Is(err, ErrReportFeedIntegrity) {
				t.Fatalf("expected write refusal, got %v", err)
			}
		})
	}
}
