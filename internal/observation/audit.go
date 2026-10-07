package observation

import "time"

type Attempt struct {
	ID         int64     `json:"id"`
	Account    string    `json:"account"`
	Endpoint   string    `json:"endpoint"`
	Reason     string    `json:"reason"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	Status     int       `json:"status"`
	RetryAfter string    `json:"retry_after,omitempty"`
}

// Attempts returns durable request accounting without credential material or
// completion tokens. An unfinished attempt has a zero Finished time. Consumers
// paging by ID should revisit unfinished IDs after their request completes.
func (s *Store) Attempts(account string, afterID int64, limit int) ([]Attempt, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.Query("SELECT id,account,endpoint,reason,started,finished,status,retry_after FROM observation_attempts WHERE account=? AND id>? ORDER BY id LIMIT ?", account, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		var started, finished int64
		if err := rows.Scan(&a.ID, &a.Account, &a.Endpoint, &a.Reason, &started, &finished, &a.Status, &a.RetryAfter); err != nil {
			return nil, err
		}
		a.Started, a.Finished = fromNS(started), fromNS(finished)
		out = append(out, a)
	}
	return out, rows.Err()
}
