package usagecoverage

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

// Keep SQL write-failure tests independent of feed schema-integrity tests.
// Only the exact INSERT is denied; metadata/schema reads continue normally.
func rejectSQLInsert(t *testing.T, db *sql.DB, table string) func() {
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
		c.RegisterAuthorizer(func(op int, name, column, database string) int {
			if op == sqlite3.SQLITE_INSERT && database == "main" && name == table {
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
				t.Errorf("SQL fault did not reach INSERT into %s", table)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}
