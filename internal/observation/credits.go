package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

const CreditMaxAge = time.Hour

// CreditStore borrows its own database. The measurement feed guards the main
// state database's DDL generation, so upgrades must not add tables there.
type CreditStore struct{ store *Store }

func OpenCredits(db *sql.DB, clock func() time.Time) (*CreditStore, error) {
	if db == nil {
		return nil, errors.New("observation: nil credit database")
	}
	if clock == nil {
		clock = time.Now
	}
	s := &Store{db: db, clock: clock}
	err := s.write(func(conn *sql.Conn) error {
		_, err := conn.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS observation_credits (
 account TEXT PRIMARY KEY, state TEXT NOT NULL, observed INTEGER NOT NULL, balance TEXT NOT NULL DEFAULT '')`)
		if err != nil {
			return err
		}
		rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info(observation_credits)`)
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var id, notNull, primary int
			var name, kind string
			var defaultValue any
			if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primary); err != nil {
				rows.Close()
				return err
			}
			found = found || name == "balance"
		}
		err = rows.Err()
		rows.Close()
		if err != nil || found {
			return err
		}
		_, err = conn.ExecContext(context.Background(), `ALTER TABLE observation_credits ADD COLUMN balance TEXT NOT NULL DEFAULT ''`)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &CreditStore{store: s}, nil
}

// Credits is separate from subscription windows: no percentage, reset, dollar
// conversion or inference guarantee can be derived from provider credit flags.
type Credits struct {
	State     string `json:"state"`
	ReadMS    int64  `json:"read_ms,omitempty"`
	ExpiresMS int64  `json:"expires_ms,omitempty"`
	Source    string `json:"source"`
	// Provider-reported credits, preserved as decimal text without currency conversion.
	Balance string `json:"balance,omitempty"`
}

var creditDecimal = regexp.MustCompile(`^[0-9]{1,18}(\.[0-9]{1,18})?$`)

func creditBalance(raw string) string {
	if !creditDecimal.MatchString(raw) {
		return ""
	}
	parts := strings.SplitN(raw, ".", 2)
	whole := strings.TrimLeft(parts[0], "0")
	if whole == "" {
		whole = "0"
	}
	if len(parts) == 2 {
		if fraction := strings.TrimRight(parts[1], "0"); fraction != "" {
			return whole + "." + fraction
		}
	}
	return whole
}

// CodexCredits consumes the same successful wham/usage read as API. A missing
// or malformed field replaces earlier certainty with unknown, never with zero.
// Routing continues to use provider flags, independently of the optional balance.
func CodexCredits(body []byte, at time.Time) Credits {
	c := Credits{State: "unknown", ReadMS: at.UnixMilli(), ExpiresMS: at.Add(CreditMaxAge).UnixMilli(), Source: APISource}
	var v struct {
		Credits *struct {
			Has       *bool           `json:"has_credits"`
			Unlimited *bool           `json:"unlimited"`
			Balance   json.RawMessage `json:"balance"`
		} `json:"credits"`
	}
	if json.Unmarshal(body, &v) != nil || v.Credits == nil || v.Credits.Has == nil || v.Credits.Unlimited == nil {
		return c
	}
	switch {
	case *v.Credits.Unlimited:
		c.State = "unlimited"
	case *v.Credits.Has:
		c.State = "available"
	default:
		c.State = "empty"
	}
	if c.State != "unlimited" {
		raw := string(v.Credits.Balance)
		if len(raw) > 0 && raw[0] == '"' {
			if json.Unmarshal(v.Credits.Balance, &raw) != nil {
				return c
			}
		}
		c.Balance = creditBalance(raw)
	}
	return c
}

// At preserves the source reading's expiry across API reads and response headers.
func (c Credits) At(now time.Time) Credits {
	if c.State != "unknown" && (c.ReadMS <= 0 || now.UnixMilli() < c.ReadMS || now.UnixMilli() >= c.ExpiresMS) {
		c.State = "stale"
	}
	return c
}

// ObserveCredits accepts only newer source evidence. Aliases use the verified
// account identity, just like quota windows. Conflicting equal-time readings
// become unknown; neither arrival order can turn them into confirmed capacity.
func (s *CreditStore) ObserveCredits(account string, sample Credits) (bool, error) {
	if account == "" || sample.ReadMS <= 0 || sample.ReadMS > s.store.clock().UnixMilli() || sample.Source != APISource {
		return false, errors.New("observation: invalid credit observation")
	}
	switch sample.State {
	case "available", "empty", "unlimited", "unknown":
	default:
		return false, errors.New("observation: invalid credit state")
	}
	if sample.Balance != "" && creditBalance(sample.Balance) != sample.Balance {
		return false, errors.New("observation: invalid credit balance")
	}
	if sample.State == "unknown" || sample.State == "unlimited" {
		sample.Balance = ""
	}
	changed := false
	err := s.store.write(func(conn *sql.Conn) error {
		result, err := conn.ExecContext(context.Background(), `INSERT INTO observation_credits(account,state,observed,balance) VALUES(?,?,?,?)
 ON CONFLICT(account) DO UPDATE SET
 state=CASE WHEN excluded.observed=observation_credits.observed THEN 'unknown' ELSE excluded.state END,
 balance=CASE WHEN excluded.observed=observation_credits.observed THEN '' ELSE excluded.balance END,
 observed=excluded.observed
 WHERE excluded.observed > observation_credits.observed OR
 (excluded.observed=observation_credits.observed AND observation_credits.state!='unknown' AND
 (excluded.state!=observation_credits.state OR excluded.balance!=observation_credits.balance))`, account, sample.State, sample.ReadMS, sample.Balance)
		if err == nil {
			n, e := result.RowsAffected()
			changed, err = n > 0, e
		}
		return err
	})
	return changed, err
}

func (s *CreditStore) ReadCredits(account string) (Credits, error) {
	c := Credits{State: "unknown", Source: APISource}
	err := s.store.db.QueryRow("SELECT state,observed,balance FROM observation_credits WHERE account=?", account).Scan(&c.State, &c.ReadMS, &c.Balance)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err == nil {
		c.ExpiresMS = time.UnixMilli(c.ReadMS).Add(CreditMaxAge).UnixMilli()
	}
	return c, err
}
