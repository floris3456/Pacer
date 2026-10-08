package historyreports

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPreparedForwardNeverInitializesAndChecksIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	if s, e := OpenPreparedForward(ctx, missing); e == nil {
		s.Close()
		t.Fatal("created missing runtime database")
	}
	if _, e := os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("runtime created file", e)
	}
	empty := filepath.Join(dir, "empty.sqlite")
	if e := os.WriteFile(empty, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if s, e := OpenPreparedForward(ctx, empty); e == nil {
		s.Close()
		t.Fatal("initialized empty runtime database")
	}
	info, e := os.Stat(empty)
	if e != nil || info.Size() != 0 {
		t.Fatal(info, e)
	}
	s, path := forwardLedger(t)
	book := forwardBook(t, s)
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	r, e := OpenPreparedForward(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	q, e := r.NativeForwardSelection(ctx, book)
	if e != nil || q.ProfileRevision != 0 {
		t.Fatal(q, e)
	}
	link := filepath.Join(dir, "hardlink.sqlite")
	if e = os.Link(path, link); e != nil {
		t.Fatal(e)
	}
	if e = CheckForwardPath(path, link); e == nil {
		t.Fatal("hard-link source alias accepted")
	}
	sym := filepath.Join(dir, "symlink.sqlite")
	if e = os.Symlink(path, sym); e != nil {
		t.Fatal(e)
	}
	if _, e = OpenPreparedForward(ctx, sym); e == nil {
		t.Fatal("symlink accepted")
	}
	if _, e = r.NativeForwardSelection(ctx, "missing"); e == nil {
		t.Fatal("missing authorized book became default")
	}
	if _, e = r.db.Exec(`DROP INDEX forward_native_page`); e != nil {
		t.Fatal(e)
	}
	if opened, e := OpenPreparedForward(ctx, path); e == nil {
		opened.Close()
		t.Fatal("runtime accepted absent keyset index")
	}
	var indexes int
	if e = r.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='forward_native_page'`).Scan(&indexes); e != nil || indexes != 0 {
		t.Fatal("runtime rebuilt an old-table index", e)
	}
	other := filepath.Join(dir, "other.sqlite")
	db, e := sql.Open("sqlite3", other)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`CREATE TABLE unrelated(id INTEGER PRIMARY KEY)`); e != nil {
		t.Fatal(e)
	}
	db.Close()
	if _, e = OpenPreparedForward(ctx, other); e == nil {
		t.Fatal("unprepared schema accepted")
	}
}
