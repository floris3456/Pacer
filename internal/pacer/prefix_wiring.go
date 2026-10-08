package pacer

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/measurement"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/prefixevidence"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Prefix evidence is optional. Every helper here only copies or projects
// already-persisted Pacer state; none of them changes routing, policy receipts,
// the usage record or the cache-activity tracker.

// prefixFact maps one persisted native record to the prefixevidence fact. The
// caller must have checked that r.Native is non-nil.
func prefixFact(r measurement.Record) prefixevidence.Fact {
	a := r.Attribution
	f := prefixevidence.Fact{
		AttemptID:    r.AttemptID,
		Account:      a.AccountID,
		Caller:       a.CallerID,
		Installation: a.InstallationID,
		Session:      a.SessionID,
		Route:        a.ProviderRoute,
		Epoch:        r.Policy.ContextEpoch,
		Revision:     r.Policy.Revision,
		SelectedTTL:  string(r.Policy.SelectedTTL),
		Kind:         prefixKind(r.Kind),
		Write5m:      copyPrefixCounter(r.Usage.CacheCreate5m),
		Write1h:      copyPrefixCounter(r.Usage.CacheCreate1h),
		Snapshot:     copyPrefixSnapshot(r.Native.PrefixSnapshot),
	}
	f.BindingRef = r.Native.CredentialBindingRef
	f.Dispatched = r.Native.Dispatched
	// A fallback start (requested_at_unverified) is not a measured t0.
	if r.StartTimeBasis == "" || r.StartTimeBasis == "upstream" {
		f.StartedAt = r.StartedAt
	}
	return f
}

// prefixKind maps record kinds onto the two kinds prefixevidence qualifies.
// Every other kind (title, summary, other) is unsupported and never observed.
func prefixKind(kind string) string {
	switch kind {
	case "primary":
		return prefixevidence.KindPrimary
	case "warming":
		return prefixevidence.KindWarming
	default:
		return "other"
	}
}

// copyPrefixCounter keeps absent counters absent; it never turns nil into zero.
func copyPrefixCounter(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// copyPrefixSnapshot deep-copies the snapshot so later changes to the persisted
// record cannot alter an observation. Nil slices stay nil.
func copyPrefixSnapshot(s *measurement.PrefixSnapshot) *prefixevidence.Snapshot {
	if s == nil {
		return nil
	}
	out := &prefixevidence.Snapshot{
		Version:     s.Version,
		KeyID:       s.KeyID,
		SettingsTag: s.SettingsTag,
		Automatic:   s.Automatic,
		Shape:       s.Shape,
	}
	if s.Endpoints != nil {
		out.Endpoints = make([]prefixevidence.Endpoint, len(s.Endpoints))
		for i, e := range s.Endpoints {
			out.Endpoints[i] = prefixevidence.Endpoint{Ordinal: e.Ordinal, Section: e.Section, TTL: e.TTL, Tag: e.Tag}
		}
	}
	if s.Reasons != nil {
		out.Reasons = append([]string(nil), s.Reasons...)
	}
	return out
}

// prefixHookTimeout bounds one prefix hook call. A call that overruns it fails
// as a metric error only; it never changes the delivered response.
const prefixHookTimeout = 2 * time.Second

// prefixHookContext is the context for one hook call. It is bounded, unlike
// the background context the hooks used before.
func prefixHookContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), prefixHookTimeout)
}

// prefixStore returns the live prefix store, or nil. Hooks call it once and use
// the returned pointer for that call only. Close clears the pointer under p.mu,
// so a hook sees the store or nil, never a pointer cleared mid-read.
func (p *Plugin) prefixStore() *prefixevidence.Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prefixEvidence
}

// observePrefix projects one persisted record into the prefix store. Like the
// other hooks, it is optional and never acknowledges or fails delivery.
func (p *Plugin) observePrefix(record measurement.Record) {
	store := p.prefixStore()
	if store == nil || record.Policy.SelectedTTL == "" || record.Native == nil || record.Native.PrefixSnapshot == nil {
		return
	}
	ctx, cancel := prefixHookContext()
	defer cancel()
	p.metricError(store.Observe(ctx, prefixFact(record)))
}

// settlePrefixAttempt forwards the CPA physical finish outcome. Logical
// CompleteRequest settlements are deliberately not forwarded: they are not
// physical outcomes.
func (p *Plugin) settlePrefixAttempt(raw []byte) {
	store := p.prefixStore()
	if store == nil {
		return
	}
	var request pluginapi.CachePolicyAttemptRequest
	if json.Unmarshal(raw, &request) != nil || request.Operation != "finish" {
		return
	}
	switch outcome := prefixevidence.Outcome(request.Outcome); outcome {
	case prefixevidence.OutcomeSucceeded, prefixevidence.OutcomeFailed, prefixevidence.OutcomeCanceled:
		ctx, cancel := prefixHookContext()
		defer cancel()
		p.metricError(store.Settle(ctx, request.AttemptID, outcome))
	}
}

// augmentPrefixReadback adds prefix_cache_evidence to a fresh, authorized GET
// 200 policy state. On any failure the caller's response is returned unchanged.
func (p *Plugin) augmentPrefixReadback(raw []byte, result any) any {
	store := p.prefixStore()
	if store == nil {
		return result
	}
	var request pluginapi.CachePolicyRequest
	if json.Unmarshal(raw, &request) != nil || request.Method != http.MethodGet {
		return result
	}
	response, ok := result.(pluginapi.CachePolicyResponse)
	if !ok || response.StatusCode != http.StatusOK {
		return result
	}
	ctx, cancel := prefixHookContext()
	defer cancel()
	body, err := store.AugmentAuthorized(ctx, request.Scope.CallerScope, response.Body)
	if err != nil {
		p.metricError(err)
		return result
	}
	response.Body = body
	return response
}

// prefixEvidencePath is the dedicated prefix-evidence database beside state_path.
func prefixEvidencePath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "prefix-evidence.sqlite")
}

// prefixDSN builds the SQLite URI for a path. The path is escaped as a URI path,
// so a '?' or '#' in a directory name is not read as the start of the query.
// It mirrors historyreports' uri helper.
func prefixDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String() + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL"
}

// openPrefixEvidence opens the optional prefix-evidence store in its own file.
// Any failure returns (nil, nil): the evidence is optional and never blocks
// startup, policy or measurement. The file is owner-only, because it holds
// account, binding and session references.
func openPrefixEvidence(statePath string, now func() time.Time) (*sql.DB, *prefixevidence.Store) {
	if statePath == "" {
		// filepath.Dir("") is ".", which would put the database in the working directory.
		return nil, nil
	}
	db := openOwnerOnlySQLite(prefixEvidencePath(statePath))
	if db == nil {
		return nil, nil
	}
	store, err := prefixevidence.Open(db, now)
	if err != nil {
		db.Close()
		return nil, nil
	}
	return db, store
}

// openOwnerOnlySQLite opens an optional evidence database in its own file, or
// returns nil on any failure.
func openOwnerOnlySQLite(path string) *sql.DB {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil
	}
	if err = file.Close(); err != nil {
		return nil
	}
	// An existing file keeps its mode, so tighten anything wider than 0600. If that
	// fails, do not open the file at all.
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Mode().Perm()&^0600 != 0 {
		if err = os.Chmod(path, 0600); err != nil {
			return nil
		}
	}
	db, err := sql.Open("sqlite3", prefixDSN(path))
	if err != nil {
		return nil
	}
	db.SetMaxOpenConns(1)
	return db
}
