package measurement

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPhysicalBootstrapIsExplicitMetadataOnlyAndRejectsPartialSchema(t *testing.T) {
	for _, missing := range []string{"table", "index", "version", "meta-row"} {
		t.Run(missing, func(t *testing.T) {
			s, db := openTest(t, filepath.Join(t.TempDir(), "physical.sqlite"))
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'measurement_physical_%'`).Scan(&n); err != nil || n != 0 {
				t.Fatal("Open eagerly created physical schema", n, err)
			}
			if err := s.StartSourceConsumer(ctx, "listener-1", "source", base); err != nil {
				t.Fatal(err)
			}
			if _, err := s.NewPhysicalProof(ctx, "listener-1"); err != nil {
				t.Fatal(err)
			}
			// Corrupt primary payload proves bootstrap does not parse event history.
			if _, err := db.Exec(`INSERT INTO measurement_records VALUES('corrupt','source','attempt','claude','a',1,2,1,X'00',1,X'00',X'00')`); err != nil {
				t.Fatal(err)
			}
			if err := s.StartSourceConsumer(ctx, "listener-2", "source", base.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.NewPhysicalProof(ctx, "listener-2"); err != nil {
				t.Fatal("startup scanned event bodies", err)
			}
			command := map[string]string{"table": "DROP TABLE measurement_physical_cuts", "index": "DROP INDEX measurement_physical_owner", "version": "UPDATE measurement_physical_meta SET version=99", "meta-row": "DELETE FROM measurement_physical_meta"}[missing]
			if _, err := db.Exec(command); err != nil {
				t.Fatal(err)
			}
			if _, err := s.NewPhysicalProof(ctx, "listener-2"); err == nil {
				t.Fatal("partial namespace recreated")
			}
			if missing == "table" {
				if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='measurement_physical_cuts'`).Scan(&n); err != nil || n != 0 {
					t.Fatal("lost proof table silently rebuilt", n, err)
				}
			}
		})
	}
}

func TestPhysicalFixedLimitsPoisonDurablyWithoutEviction(t *testing.T) {
	s, db := openTest(t, filepath.Join(t.TempDir(), "limit.sqlite"))
	if err := s.StartSourceConsumer(ctx, "listener", "source", base); err != nil {
		t.Fatal(err)
	}
	p, err := s.NewPhysicalProof(ctx, "listener")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE measurement_physical_meta SET gaps=8192`); err != nil {
		t.Fatal(err)
	}
	ref := SourceReference("source", SourceScope{Provider: "claude", AuthID: "dummy", AuthIndex: "index", Kind: ClaudeHTTPSource, Version: 1})
	if err = p.Invalidate(ctx, ref, "manager", ""); !errors.Is(err, ErrLimit) {
		t.Fatal("limit not rejected", err)
	}
	if _, err = s.NewPhysicalProof(ctx, "listener"); !errors.Is(err, ErrLimit) {
		t.Fatal("namespace exhaustion healed on reopen", err)
	}
	if err = p.Fault(ctx, "a"); !errors.Is(err, ErrLimit) {
		t.Fatal("later mutation cleared exhaustion", err)
	}
}
