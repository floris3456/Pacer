package cachepolicy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Store borrows db; its owner controls its lifetime and SQLite durability/locking
// settings. Every mutation obtains SQLite's write lock before reading state.
type Store struct {
	db     *sql.DB
	clock  func() time.Time
	routes map[string]map[TTL]bool
}

const schema = `
CREATE TABLE cache_policy_meta (id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
INSERT INTO cache_policy_meta VALUES(1,1);
CREATE TABLE cache_policy_scopes (
 scope_key TEXT PRIMARY KEY, caller_digest TEXT NOT NULL,
 installation_id TEXT NOT NULL, session_id TEXT NOT NULL, provider_route TEXT NOT NULL,
 capability_hash TEXT NOT NULL, state BLOB NOT NULL,
 UNIQUE(caller_digest,installation_id,session_id,provider_route)
);
CREATE TABLE cache_policy_operations (
 caller_digest TEXT NOT NULL, operation_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 scope_key TEXT NOT NULL, capability_hash TEXT NOT NULL,
 result BLOB, error_status INTEGER, error_code TEXT, error_message TEXT,
 PRIMARY KEY(caller_digest,operation_id)
);
CREATE TABLE cache_policy_epochs (
 scope_key TEXT NOT NULL, context_epoch TEXT NOT NULL,
 PRIMARY KEY(scope_key,context_epoch)
);
CREATE TABLE cache_policy_requests (
 scope_key TEXT NOT NULL, request_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 snapshot BLOB NOT NULL, kind TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(scope_key,request_id)
);
CREATE TABLE cache_policy_attempts (
 scope_key TEXT NOT NULL, attempt_id TEXT NOT NULL, request_id TEXT NOT NULL,
 snapshot BLOB NOT NULL, outcome TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(scope_key,attempt_id)
);
CREATE INDEX cache_policy_attempt_request ON cache_policy_attempts(scope_key,request_id,outcome);
`

func configured(db *sql.DB, opts Options) (*Store, error) {
	if db == nil || opts.Clock == nil {
		return nil, invalid("A database and clock are required.")
	}
	s := &Store{db: db, clock: opts.Clock, routes: make(map[string]map[TTL]bool)}
	for _, r := range opts.Routes {
		if !identifier(r.ProviderRoute) || len(r.TTLs) == 0 || s.routes[r.ProviderRoute] != nil {
			return nil, invalid("Route declarations must be unique and explicitly support TTLs.")
		}
		s.routes[r.ProviderRoute] = make(map[TTL]bool)
		for _, ttl := range r.TTLs {
			if ttl != TTL5m && ttl != TTL1h {
				return nil, invalid("Unsupported route TTL.")
			}
			s.routes[r.ProviderRoute][ttl] = true
		}
	}
	return s, nil
}

// Create explicitly initializes a never-enrolled installation. It refuses an
// existing schema. Normal startup MUST use Open, including after storage loss.
func Create(ctx context.Context, db *sql.DB, opts Options) (*Store, error) {
	s, err := configured(db, opts)
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, unavailable()
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return nil, unavailable()
	}
	if err = tx.Commit(); err != nil {
		return nil, unavailable()
	}
	return s, nil
}

// Open never initializes or clears state. Outstanding requests survive restart
// and must be explicitly settled by the host after confirming dispatch ceased.
func Open(ctx context.Context, db *sql.DB, opts Options) (*Store, error) {
	s, err := configured(db, opts)
	if err != nil {
		return nil, err
	}
	var version int
	if err = db.QueryRowContext(ctx, `SELECT version FROM cache_policy_meta WHERE id=1`).Scan(&version); err != nil || version != 1 {
		return nil, unavailable()
	}
	// Verify all correctness tables exist even when no scopes have enrolled yet.
	for _, table := range []string{"cache_policy_scopes", "cache_policy_operations", "cache_policy_epochs", "cache_policy_requests", "cache_policy_attempts"} {
		var n int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			return nil, unavailable()
		}
	}
	return s, nil
}

func (s *Store) now() time.Time { return s.clock().UTC() }

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable()
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE cache_policy_meta SET version=version WHERE id=1 AND version=1`)
	if err != nil {
		return unavailable()
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return unavailable()
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return unavailable()
	}
	return nil
}

func identifier(v string) bool {
	if len(v) < 1 || len(v) > 256 || strings.TrimSpace(v) != v {
		return false
	}
	for _, r := range v {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func validScope(scope Scope) bool {
	return identifier(scope.InstallationID) && identifier(scope.SessionID) && identifier(scope.ProviderRoute)
}

func credentials(c Credentials, scope Scope) (key, capHash string, err error) {
	// SHA-256 caller digests use lowercase hex; raw API keys must never arrive here.
	b, e := hex.DecodeString(c.CallerDigest)
	if e != nil || len(b) != 32 || strings.ToLower(c.CallerDigest) != c.CallerDigest {
		return "", "", denied()
	}
	capability, e := base64.RawURLEncoding.DecodeString(c.Capability)
	if e != nil || len(capability) != 32 || base64.RawURLEncoding.EncodeToString(capability) != c.Capability {
		return "", "", denied()
	}
	if !validScope(scope) {
		return "", "", invalid("Invalid scope identifiers.")
	}
	key = digest(struct {
		Caller string
		Scope  Scope
	}{c.CallerDigest, scope})
	h := sha256.Sum256(capability)
	return key, hex.EncodeToString(h[:]), nil
}

func digest(value any) string {
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", unavailable()
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func load(ctx context.Context, q queryer, key, capHash string) (State, error) {
	var state State
	var version int
	if err := q.QueryRowContext(ctx, `SELECT version FROM cache_policy_meta WHERE id=1`).Scan(&version); err != nil || version != 1 {
		return state, unavailable()
	}
	var raw []byte
	var storedHash, caller string
	var scope Scope
	err := q.QueryRowContext(ctx, `SELECT capability_hash,state,caller_digest,installation_id,session_id,provider_route FROM cache_policy_scopes WHERE scope_key=?`, key).Scan(&storedHash, &raw, &caller, &scope.InstallationID, &scope.SessionID, &scope.ProviderRoute)
	if errors.Is(err, sql.ErrNoRows) {
		return state, conflict("state_missing", "No durable policy exists; managed requests cannot enroll implicitly.")
	}
	if err != nil {
		return state, unavailable()
	}
	if subtle.ConstantTimeCompare([]byte(capHash), []byte(storedHash)) != 1 {
		return state, denied()
	}
	if json.Unmarshal(raw, &state) != nil || !validState(state) || state.Scope != scope || key != digest(struct {
		Caller string
		Scope  Scope
	}{caller, scope}) {
		return State{}, unavailable()
	}
	var epochs int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM cache_policy_epochs WHERE scope_key=? AND context_epoch=?`, key, state.Active.ContextEpoch).Scan(&epochs); err != nil || epochs != 1 {
		return State{}, unavailable()
	}
	return state, nil
}

func validState(state State) bool {
	if state.Protocol != Protocol || !validScope(state.Scope) || state.Revision < 1 || !identifier(state.Active.ContextEpoch) || state.Active.Checkpoint != state.Active.ContextEpoch {
		return false
	}
	if state.EnrollmentRequired {
		if state.Active.TTL != "" || !state.Active.Used || state.ActivePrimary != 0 || state.ActiveWarming != 0 || state.ActiveAttempts != 0 || state.LastCommit != nil {
			return false
		}
	} else if state.Active.TTL != TTL5m && state.Active.TTL != TTL1h {
		return false
	}
	if p := state.LastCommit; p != nil {
		if !validReceipt(p.Receipt) || p.Receipt.Checkpoint != state.Active.Checkpoint || p.AcknowledgedAt.IsZero() || !state.Active.Used {
			return false
		}
	}
	if state.ActivePrimary < 0 || state.ActiveWarming < 0 || state.ActiveAuxiliary < 0 || state.ActiveAttempts < 0 {
		return false
	}
	if (state.ActivePrimary > 0 || state.ActiveWarming > 0 || state.ActiveAttempts > 0) && !state.Active.Used {
		return false
	}
	if p := state.Pending; p != nil {
		if !identifier(p.ID) || p.FromEpoch != state.Active.ContextEpoch || p.FromCheckpoint != state.Active.Checkpoint || p.PreparedRevision != state.Revision || p.PreparedAt.IsZero() || (p.TTL != TTL5m && p.TTL != TTL1h) {
			return false
		}
	}
	return state.WarmingPaused == (state.Pending != nil || state.EnrollmentRequired)
}

func save(ctx context.Context, tx *sql.Tx, key string, state State) error {
	if !validState(state) {
		return unavailable()
	}
	b, err := json.Marshal(state)
	if err != nil {
		return unavailable()
	}
	r, err := tx.ExecContext(ctx, `UPDATE cache_policy_scopes SET state=? WHERE scope_key=?`, b, key)
	if err != nil {
		return unavailable()
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return unavailable()
	}
	return nil
}

func (s *Store) support(route string, ttl TTL) error {
	if !s.routes[route][ttl] {
		return failure(422, "unsupported", "This provider route does not support the requested cache policy.")
	}
	return nil
}

// Get is read-only, including while a transition or request is active.
func (s *Store) Get(ctx context.Context, c Credentials, scope Scope) (State, error) {
	key, hash, err := credentials(c, scope)
	if err != nil {
		return State{}, err
	}
	return load(ctx, s.db, key, hash)
}
