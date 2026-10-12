package observation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const CreditMaxAge = time.Hour

// Credits is separate from subscription windows: no percentage, reset, dollar
// conversion or inference guarantee can be derived from provider credit flags.
type Credits struct {
	State     string `json:"state"`
	ReadMS    int64  `json:"read_ms,omitempty"`
	ExpiresMS int64  `json:"expires_ms,omitempty"`
	Source    string `json:"source"`
}

// CodexCredits consumes the same successful wham/usage read as API. A missing
// or malformed field replaces earlier certainty with unknown, never with zero.
// Balance units are not part of this contract; only provider flags are used.
func CodexCredits(body []byte, at time.Time) Credits {
	c := Credits{State: "unknown", ReadMS: at.UnixMilli(), ExpiresMS: at.Add(CreditMaxAge).UnixMilli(), Source: APISource}
	var v struct {
		Credits *struct {
			Has       *bool `json:"has_credits"`
			Unlimited *bool `json:"unlimited"`
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
func (s *Store) ObserveCredits(account string, sample Credits) (bool, error) {
	if account == "" || sample.ReadMS <= 0 || sample.ReadMS > s.clock().UnixMilli() || sample.Source != APISource {
		return false, errors.New("observation: invalid credit observation")
	}
	switch sample.State {
	case "available", "empty", "unlimited", "unknown":
	default:
		return false, errors.New("observation: invalid credit state")
	}
	changed := false
	err := s.write(func(conn *sql.Conn) error {
		result, err := conn.ExecContext(context.Background(), `INSERT INTO observation_credits(account,state,observed) VALUES(?,?,?)
 ON CONFLICT(account) DO UPDATE SET
 state=CASE WHEN excluded.observed=observation_credits.observed THEN 'unknown' ELSE excluded.state END,
 observed=excluded.observed
 WHERE excluded.observed > observation_credits.observed OR
 (excluded.observed=observation_credits.observed AND observation_credits.state!='unknown' AND excluded.state!=observation_credits.state)`, account, sample.State, sample.ReadMS)
		if err == nil {
			n, e := result.RowsAffected()
			changed, err = n > 0, e
		}
		return err
	})
	return changed, err
}

func (s *Store) ReadCredits(account string) (Credits, error) {
	c := Credits{State: "unknown", Source: APISource}
	err := s.db.QueryRow("SELECT state,observed FROM observation_credits WHERE account=?", account).Scan(&c.State, &c.ReadMS)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err == nil {
		c.ExpiresMS = time.UnixMilli(c.ReadMS).Add(CreditMaxAge).UnixMilli()
	}
	return c, err
}
