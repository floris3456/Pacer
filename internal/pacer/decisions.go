package pacer

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/routing"
)

// RequestID and ActualAccount are additive JSON fields inside the existing body
// column; the pacer_decisions DDL is unchanged. They let the dry-run report
// join a decision to its priced usage and compare billing accounts, not aliases.
type decisionEntry struct {
	At              time.Time        `json:"at"`
	Chat            string           `json:"chat"`
	RequestID       string           `json:"request_id,omitempty"`
	Provider        string           `json:"provider"`
	Model           string           `json:"model"`
	PreviousAccount string           `json:"previous_account,omitempty"`
	ActualAuth      string           `json:"actual_auth"`
	ActualAccount   string           `json:"actual_account,omitempty"`
	Proposed        routing.Decision `json:"proposed"`
	Outcome         string           `json:"outcome"`
}

func openDecisions(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS pacer_decisions (seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, body BLOB NOT NULL); CREATE INDEX IF NOT EXISTS pacer_decisions_at ON pacer_decisions(at)`)
	return err
}

func (p *Plugin) recordDecision(request string, t attempt, c credential, outcome string) {
	p.mu.Lock()
	// With routing off, Pacer acts only to keep a chat on its account.
	enabled := p.cfg.Routing.Mode == "on" || t.Decision.Reason == routing.KeepAccount
	p.mu.Unlock()
	if !enabled || t.Key == "" || t.Decision.Reason == "" {
		return
	}
	entry := decisionEntry{At: p.now(), Chat: t.Key, RequestID: request, Provider: c.Provider, Model: t.Model, PreviousAccount: t.PreviousAccount, ActualAuth: c.ID, ActualAccount: c.Account, Proposed: t.Decision, Outcome: outcome}
	body, err := json.Marshal(entry)
	if err != nil {
		return
	}
	// Trace failures never change serving policy; they are counted and exposed.
	_, err = p.db.Exec(`INSERT INTO pacer_decisions(at,body) VALUES(?,?)`, entry.At.UnixNano(), body)
	p.mu.Lock()
	if err != nil {
		p.decisionErrors++
	}
	p.mu.Unlock()
}

func (p *Plugin) decisionView() ([]decisionEntry, error) {
	rows, err := p.db.Query(`SELECT body FROM pacer_decisions ORDER BY seq DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []decisionEntry{}
	for rows.Next() {
		var body []byte
		if err = rows.Scan(&body); err != nil {
			return nil, err
		}
		var entry decisionEntry
		if err = json.Unmarshal(body, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
