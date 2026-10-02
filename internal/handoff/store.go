package handoff

import (
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
)

func openStore(path string) (*sql.DB, State, error) {
	state := State{Accounts: map[string]Account{}, Bindings: map[string]*Binding{}}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, state, err
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		return nil, state, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL, data BLOB NOT NULL)`); err != nil {
		db.Close()
		return nil, state, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, state, err
	}
	var data []byte
	var version int
	err = db.QueryRow(`SELECT version,data FROM state WHERE id=1`).Scan(&version, &data)
	if err == sql.ErrNoRows {
		return db, state, nil
	}
	if err != nil {
		db.Close()
		return nil, state, err
	}
	if version != 1 {
		db.Close()
		return nil, state, fmt.Errorf("unsupported quota state version %d", version)
	}
	if err = json.Unmarshal(data, &state); err != nil {
		db.Close()
		return nil, state, err
	}
	return db, state, nil
}

func (e *Engine) save() error {
	data, err := json.Marshal(e.state)
	if err != nil {
		return err
	}
	_, err = e.db.Exec(`INSERT INTO state(id,version,data) VALUES(1,1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, data)
	// A persistence error poisons this instance: no admission may use uncommitted state.
	if err != nil {
		e.broken = err
	}
	return err
}
