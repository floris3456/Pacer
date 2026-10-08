package measurement

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSourceSchemaAdditiveVersionOneUpgradeAndMissingTableRefusal(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "v1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO measurement_delivery_runs VALUES('legacy',1,2,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, db, Options{}); err != nil {
		t.Fatal(err)
	}
	var version, count int
	if err = db.QueryRow(`SELECT version FROM measurement_meta WHERE id=1`).Scan(&version); err != nil || version != 2 {
		t.Fatal(version, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM measurement_delivery_runs WHERE id='legacy'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err = db.Exec(`DROP TABLE measurement_source_gaps`); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(ctx, db, Options{}); err == nil {
		t.Fatal("missing evidence table silently recreated")
	}
}
