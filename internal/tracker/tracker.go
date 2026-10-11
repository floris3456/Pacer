// Package tracker owns durable chat affinity and the existing forced-move
// checkpoint contract. Cache-policy epochs deliberately live elsewhere.
package tracker

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Claim struct {
	Protocol        int    `json:"protocol"`
	Installation    string `json:"installation"`
	Session         string `json:"session"`
	Route           string `json:"route"`
	Request         string `json:"request"`
	Kind            string `json:"kind"`
	Capability      string `json:"capability"`
	Checkpoint      string `json:"checkpoint,omitempty"`
	Auto            bool   `json:"auto"`
	Fresh           bool   `json:"fresh"`
	CompactOnSwitch bool   `json:"compact_on_switch,omitempty"`
}

func (c Claim) Valid() bool {
	if c.Protocol != 1 || c.Installation == "" || c.Session == "" || c.Route == "" || c.Request == "" || len(c.Capability) != 64 {
		return false
	}
	if _, err := hex.DecodeString(c.Capability); err != nil {
		return false
	}
	for _, v := range []string{c.Installation, c.Session, c.Route, c.Request, c.Checkpoint} {
		if len(v) > 256 {
			return false
		}
	}
	return c.Kind == "primary" || c.Kind == "compaction" || c.Kind == "title" || c.Kind == "generate"
}

func Digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func Key(caller, provider string, c Claim) string {
	return Digest(caller + "\x00" + c.Installation + "\x00" + c.Session + "\x00" + provider + "\x00" + c.Route)
}

type Binding struct {
	Key            string    `json:"key"`
	Caller         string    `json:"-"`
	Capability     string    `json:"capability,omitempty"`
	Route          string    `json:"route"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	Auth           string    `json:"auth"`
	Account        string    `json:"account"`
	Alias          string    `json:"alias"`
	Generation     int       `json:"generation"`
	Consumed       string    `json:"consumed"`
	PrimaryUsed    bool      `json:"primary_used"`
	LastUsed       time.Time `json:"last_used"`
	ProtectedUntil time.Time `json:"protected_until"`
	UnknownWarmth  bool      `json:"unknown_warmth"`
}

type Flight struct {
	ID, Key, Auth, Account, Kind, Checkpoint string
	Generation                               int
	Started                                  time.Time
}

type Selection struct {
	Auth, Account, Alias, Provider, Model string
	Forced                                bool
}
type Refusal struct{ Code, Message string }

func (e *Refusal) Error() string { return e.Code + ": " + e.Message }

type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	now     func() time.Time
	flights map[string]Flight
}

func Open(db *sql.DB, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS pacer_chats (key TEXT PRIMARY KEY, capability TEXT NOT NULL, route TEXT NOT NULL, body BLOB NOT NULL)`)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS pacer_activity_contexts (request_id TEXT NOT NULL, auth TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(request_id,auth))`); err != nil {
		return nil, err
	}
	return &Store{db: db, now: now, flights: map[string]Flight{}}, nil
}

func (s *Store) read(key string) (Binding, error) {
	var raw []byte
	err := s.db.QueryRow(`SELECT body FROM pacer_chats WHERE key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, nil
	}
	if err != nil {
		return Binding{}, err
	}
	var b Binding
	err = json.Unmarshal(raw, &b)
	return b, err
}
func (s *Store) save(b Binding) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO pacer_chats(key,capability,route,body) VALUES(?,?,?,?) ON CONFLICT(key) DO UPDATE SET capability=excluded.capability,route=excluded.route,body=excluded.body`, b.Key, b.Capability, b.Route, raw)
	return err
}

func (s *Store) Get(key string) (Binding, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.read(key)
	active := false
	for _, f := range s.flights {
		if f.Key == key && f.Kind == "primary" {
			active = true
		}
	}
	return b, active, err
}

func (s *Store) Seed(key string, c Selection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.read(key)
	if err != nil || b.Key != "" {
		return err
	}
	return s.save(Binding{Key: key, Auth: c.Auth, Account: c.Account, Alias: c.Alias, Provider: c.Provider, Model: c.Model, Generation: 1, UnknownWarmth: true})
}

// Admit runs before the selected account receives a byte. A cold optimization
// is not a forced move and never requests compaction. Auxiliaries never rebind.
func (s *Store) Admit(id, key string, c Selection, q *Claim, gate bool) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.read(key)
	if err != nil {
		return Binding{}, err
	}
	kind := "primary"
	if q != nil {
		if !q.Valid() {
			return b, &Refusal{"quota_protocol_invalid", "Invalid quota coordination claim"}
		}
		if b.Capability != "" && b.Capability != q.Capability {
			return b, &Refusal{"quota_capability_conflict", "Session capability differs"}
		}
		kind = q.Kind
	}
	if kind != "primary" {
		s.flights[id] = Flight{ID: id, Key: key, Auth: c.Auth, Account: c.Account, Kind: kind, Started: s.now()}
		return b, nil
	}
	moved := b.Account != "" && b.Account != c.Account
	if moved && c.Forced && gate && q != nil && q.CompactOnSwitch && !q.Fresh && q.Auto && (q.Checkpoint == "" || q.Checkpoint == b.Consumed) {
		return b, &Refusal{"quota_compact_required", fmt.Sprintf("CPA moves this chat from %s to %s.", b.Alias, c.Alias)}
	}
	if b.Key == "" {
		b = Binding{Key: key, Generation: 1, UnknownWarmth: true}
	}
	if moved {
		b.Generation++
		b.ProtectedUntil = time.Time{}
		b.UnknownWarmth = true
	}
	b.Auth, b.Account, b.Alias, b.Provider, b.Model = c.Auth, c.Account, c.Alias, c.Provider, c.Model
	b.PrimaryUsed, b.LastUsed = true, s.now()
	if q != nil {
		if b.Consumed != q.Checkpoint {
			b.ProtectedUntil = time.Time{}
			b.UnknownWarmth = true
		}
		b.Capability, b.Route, b.Consumed = q.Capability, q.Route, q.Checkpoint
	}
	if err = s.save(b); err != nil {
		return Binding{}, err
	}
	s.flights[id] = Flight{ID: id, Key: key, Auth: c.Auth, Account: c.Account, Kind: kind, Checkpoint: b.Consumed, Generation: b.Generation, Started: s.now()}
	raw, err := json.Marshal(s.flights[id])
	if err != nil {
		return Binding{}, err
	}
	if _, err = s.db.Exec(`INSERT INTO pacer_activity_contexts(request_id,auth,body) VALUES(?,?,?) ON CONFLICT(request_id,auth) DO NOTHING`, id, c.Auth, raw); err != nil {
		return Binding{}, err
	}
	return b, nil
}

// CacheActivityForAuth is for asynchronous usage: the logical request may have
// completed and another physical retry may have selected a different account.
// Retained context fences late activity against the exact checkpoint/account.
func (s *Store) CacheActivityForAuth(id, auth string, started time.Time, success, qualifying bool, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !success || !qualifying || ttl <= 0 || started.IsZero() {
		return nil
	}
	var raw []byte
	err := s.db.QueryRow(`SELECT body FROM pacer_activity_contexts WHERE request_id=? AND auth=?`, id, auth).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var flight Flight
	if err = json.Unmarshal(raw, &flight); err != nil {
		return err
	}
	if flight.Kind != "primary" || started.Before(flight.Started) {
		return nil
	}
	b, err := s.read(flight.Key)
	if err != nil {
		return err
	}
	if b.Account != flight.Account || b.Consumed != flight.Checkpoint || b.Generation != flight.Generation {
		return nil
	}
	until := started.Add(ttl)
	if until.After(b.ProtectedUntil) {
		b.ProtectedUntil = until
	}
	b.UnknownWarmth = false
	return s.save(b)
}

// CacheActivity accepts only confirmed same-prefix read/write evidence. The
// caller must supply the effective wire TTL, not a user preference. Lifecycle
// completion and failed/title/summary requests alone do not refresh protection.
func (s *Store) CacheActivity(id string, success, qualifying bool, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flights[id]
	if !ok || f.Kind != "primary" || !success || !qualifying || ttl <= 0 {
		return nil
	}
	b, err := s.read(f.Key)
	if err != nil {
		return err
	}
	if b.Account != f.Account || b.Consumed != f.Checkpoint || b.Generation != f.Generation {
		return nil
	}
	until := f.Started.Add(ttl)
	if until.After(b.ProtectedUntil) {
		b.ProtectedUntil = until
	}
	b.UnknownWarmth = false
	return s.save(b)
}

func (s *Store) Complete(id string) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.flights, id) }

func (s *Store) ByCapability(token, route string) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Older clients may have reused a capability across callers or sessions.
	// Require one match in a single snapshot; never choose an arbitrary caller.
	rows, err := s.db.Query(`SELECT body FROM pacer_chats WHERE capability=? AND route=? LIMIT 2`, Digest(token), route)
	if err != nil {
		return Binding{}, errors.New("unknown session capability")
	}
	defer rows.Close()
	var raw []byte
	if !rows.Next() || rows.Scan(&raw) != nil {
		return Binding{}, errors.New("unknown session capability")
	}
	if rows.Next() || rows.Err() != nil {
		return Binding{}, errors.New("unknown session capability")
	}
	var b Binding
	if err = json.Unmarshal(raw, &b); err != nil {
		return Binding{}, err
	}
	return b, nil
}
