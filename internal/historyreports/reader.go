// Package historyreports computes native, pinned CPAMP-compatible reports from
// published Pacer archives. It has no HTTP, authorization or live-provider role.
package historyreports

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

const Version = "cpamp-e8492c2-archive-reports-v1"

var (
	ErrInput    = errors.New("history reports: invalid filter or reference")
	ErrBoundary = errors.New("history reports: pin exceeds frozen evidence")
	ErrSource   = errors.New("history reports: incompatible report evidence")
	ErrCursor   = errors.New("history reports: invalid or mismatched page cursor")
	ErrLimit    = errors.New("history reports: preparation limits exceeded")
)

// Preparation ceilings. Input above any of them fails with ErrLimit; totals are
// never truncated. Observations and events were raised on 2026-10-08 because a
// real 5-week CPAMP backup (278,399 observations) exceeded the earlier 100,000
// observation ceiling. Values are pinned by prepare_limits_test.go.
const (
	maxEvents           = 2000000
	maxObservations     = 2000000
	maxAccounts         = 1000
	maxProfileRevisions = 10000
	maxForecasts        = 100000
	maxForecastResults  = 200000
	maxLatestProfiles   = 1000
)

// preparationCeilings maps each row-counted table to its ceiling. Production code
// only reads it; tests lower entries to exercise the boundary and restore them.
var preparationCeilings = map[string]int64{
	"analytics_events":            maxEvents,
	"analytics_observations":      maxObservations,
	"analytics_accounts":          maxAccounts,
	"analytics_profile_revisions": maxProfileRevisions,
	"analytics_forecasts":         maxForecasts,
	"analytics_forecast_results":  maxForecastResults,
}

type Pin = cpamp.ProfileWorkspaceFilter
type Workspace = cpamp.ProfileWorkspaceReport
type Window = cpamp.QuotaUsageWindow
type Evidence = cpamp.QuotaUsageEvidence
type EvidencePin = cpamp.QuotaUsageFilter

type prepared struct {
	Version                                 string
	Boundary                                history.Boundary
	Pin                                     Pin
	CursorKey                               []byte
	TokenEvidence, Forecasts, Verifications int64
	PollIntervalSeconds                     int64
	HistoricalReads                         bool
}

// PrepareOptions freezes the source runtime setting that affects credential
// freshness. Zero selects CPAMP's documented default of 300 seconds. No polling
// is performed; deployments using ANALYTICS_POLL_INTERVAL_SECONDS must pass it.
type PrepareOptions struct{ PollIntervalSeconds int64 }
type Reader struct {
	db              *sql.DB
	service         *cpamp.Service
	meta            prepared
	path            string
	historicalGate  chan struct{}
	historicalCache map[string]historicalEntry
	historicalClock uint64
	historicalBytes int
}

// Prepare is explicit offline work. dir is trusted task-local configuration,
// never management input. The created directory belongs to this operation.
// Source tables/pins in the archive are not changed. The result is read-only.
func Prepare(ctx context.Context, archive *history.Store, dir string, options ...PrepareOptions) (*Reader, error) {
	if archive == nil {
		return nil, ErrInput
	}
	if len(options) > 1 {
		return nil, ErrInput
	}
	interval := int64(300)
	if len(options) == 1 && options[0].PollIntervalSeconds != 0 {
		interval = options[0].PollIntervalSeconds
	}
	if interval < 60 || interval > 86400 {
		return nil, ErrInput
	}
	owned, e := os.MkdirTemp(dir, "historyreports-")
	if e != nil {
		return nil, ErrSource
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(owned)
		}
	}()
	path := filepath.Join(owned, "reports.sqlite")
	b, e := archive.MaterializeReports(ctx, path)
	if e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite3", uri(path, "mode=rw&_journal_mode=DELETE&_synchronous=FULL"))
	if e != nil {
		return nil, ErrSource
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	// Fixed resource ceilings fail explicitly rather than truncating totals.
	for table, max := range preparationCeilings {
		var n int64
		if db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n) != nil {
			return nil, ErrSource
		}
		if n > max {
			return nil, ErrLimit
		}
	}
	s := cpamp.New(db, b.HMS)
	s.PollInterval = time.Duration(interval) * time.Second
	book, e := s.PrepareBook(ctx, cpamp.HistoricalImportCheckpoint{SourceCheckpoint: b.CheckpointSHA256, EventsThrough: b.EventsThrough, ObservationsThrough: b.ObservationsThrough, At: b.HMS})
	if e != nil {
		return nil, ErrSource
	}
	config, e := s.ProfileConfig(ctx, 0)
	if e != nil {
		return nil, ErrSource
	}
	if len(config.Profiles) > maxLatestProfiles {
		return nil, ErrLimit
	}
	m := prepared{Version: Version, Boundary: b, CursorKey: make([]byte, 32), PollIntervalSeconds: interval}
	if _, e = rand.Read(m.CursorKey); e != nil {
		return nil, ErrSource
	}
	if db.QueryRowContext(ctx, `SELECT coalesce((SELECT max(revision) FROM analytics_token_evidence),0),coalesce((SELECT max(rowid) FROM analytics_forecasts),0),coalesce((SELECT max(rowid) FROM analytics_forecast_results),0)`).Scan(&m.TokenEvidence, &m.Forecasts, &m.Verifications) != nil {
		return nil, ErrSource
	}
	m.Pin = Pin{ProfileFilter: cpamp.ProfileFilter{At: b.HMS, Through: b.EventsThrough, ObservationsThrough: b.ObservationsThrough, Revision: config.Revision, TokenEvidenceThrough: m.TokenEvidence, ForecastsThrough: m.Forecasts, VerificationsThrough: m.Verifications, Estimator: cpamp.EstimatorVersion, ValuationRule: book.ID}}
	if e = prepareHistoricalReads(ctx, db, s, m); e != nil {
		return nil, domainError(ctx, e)
	}
	m.HistoricalReads = true
	raw, _ := json.Marshal(m)
	if _, e = db.ExecContext(ctx, `CREATE TABLE pacer_report_preparation(id INTEGER PRIMARY KEY CHECK(id=1),payload BLOB NOT NULL);INSERT INTO pacer_report_preparation VALUES(1,?)`, raw); e != nil {
		return nil, ErrSource
	}
	if db.Close() != nil || os.Chmod(path, 0400) != nil {
		return nil, ErrSource
	}
	r, e := Open(ctx, path)
	if e != nil {
		return nil, e
	}
	success = true
	return r, nil
}

// Open reopens an already prepared artifact; it performs no import, source scan,
// provider access, schema mutation or valuation-policy activation.
func Open(ctx context.Context, path string) (*Reader, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, ErrInput
	}
	info, e := os.Lstat(abs)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0277 != 0 {
		return nil, ErrSource
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e = os.Lstat(abs + suffix); !os.IsNotExist(e) {
			return nil, ErrSource
		}
	}
	db, e := sql.Open("sqlite3", uri(abs, "mode=ro&immutable=1&_query_only=1"))
	if e != nil {
		return nil, ErrSource
	}
	db.SetMaxOpenConns(1)
	var raw []byte
	var m prepared
	if db.QueryRowContext(ctx, `SELECT payload FROM pacer_report_preparation WHERE id=1`).Scan(&raw) != nil || json.Unmarshal(raw, &m) != nil || m.Version != Version || len(m.CursorKey) != 32 || m.Pin.At != m.Boundary.HMS || m.PollIntervalSeconds < 60 || m.PollIntervalSeconds > 86400 {
		db.Close()
		return nil, ErrSource
	}
	s := cpamp.New(db, m.Boundary.HMS)
	s.PollInterval = time.Duration(m.PollIntervalSeconds) * time.Second
	return &Reader{db: db, service: s, meta: m, path: abs, historicalGate: make(chan struct{}, 1), historicalCache: map[string]historicalEntry{}}, nil
}
func (r *Reader) Close() error { return r.db.Close() }

// ArtifactPath is for trusted persistence/cleanup configuration, not response DTOs.
func (r *Reader) ArtifactPath() string { return r.path }
func (r *Reader) DefaultPin() Pin      { return r.meta.Pin }
func uri(path, options string) string {
	return (&url.URL{Scheme: "file", Path: path}).String() + "?" + options
}
func hash(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && strings.ToLower(s) == s
}
func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}
func domainError(ctx context.Context, e error) error {
	if e == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrSource
}

type Profile struct {
	Reference string `json:"reference"`
	cpamp.Profile
}
type ProfileList struct {
	Revision int64     `json:"source_revision"`
	Profiles []Profile `json:"profiles"`
}

// Profiles uses -1 for the latest frozen revision, zero for the original initial
// revision. References match history.Store.Profiles; CPAMP IDs remain in pins.
func (r *Reader) Profiles(ctx context.Context, revision int64) (ProfileList, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	if revision < -1 {
		return ProfileList{}, ErrInput
	}
	v := revision
	if v == -1 {
		v = r.meta.Pin.Revision
	}
	if v == 0 {
		v = -1
	}
	c, e := r.service.ProfileConfig(ctx, v)
	if e != nil {
		if ctx.Err() != nil {
			return ProfileList{}, ctx.Err()
		}
		return ProfileList{}, ErrInput
	}
	out := ProfileList{Revision: c.Revision, Profiles: []Profile{}}
	for _, p := range c.Profiles {
		out.Profiles = append(out.Profiles, Profile{Reference: "profile:" + hash(p.ID), Profile: p})
	}
	return out, nil
}

// Query either supplies an exact old Pin, or bounded native filters for a new
// H-anchored report. Combining these two forms is rejected rather than changing
// a saved report's meaning. RequestPage pages CPAMP's fixed 25-row request list.
type Query struct {
	Pin              *Pin         `json:"pin,omitempty"`
	EvidencePin      *EvidencePin `json:"evidence_pin,omitempty"`
	ProfileReference string       `json:"profile_reference,omitempty"` // all/unassigned also supported
	AccountReference string       `json:"account_reference,omitempty"`
	KeyHashes        []string     `json:"key_hashes,omitempty"`
	Model            string       `json:"model,omitempty"`
	From             int64        `json:"from,omitempty"`
	To               int64        `json:"to,omitempty"`
	RequestPage      int          `json:"request_page,omitempty"`
}

func (r *Reader) filter(ctx context.Context, q Query) (Pin, error) {
	f := r.meta.Pin
	if err := ctx.Err(); err != nil {
		return f, err
	}
	if q.EvidencePin != nil {
		return f, ErrInput
	}
	if q.Pin != nil {
		if q.ProfileReference != "" || q.AccountReference != "" || len(q.KeyHashes) != 0 || q.Model != "" || q.From != 0 || q.To != 0 || q.RequestPage != 0 {
			return f, ErrInput
		}
		f = *q.Pin
	} else {
		if len(q.Model) > 256 || len(q.KeyHashes) > 100 {
			return f, ErrInput
		}
		f.From, f.To, f.Model, f.Page = q.From, q.To, q.Model, q.RequestPage
		if q.ProfileReference == "all" || q.ProfileReference == "unassigned" {
			f.Profile = q.ProfileReference
		} else if q.ProfileReference != "" {
			ps, e := r.Profiles(ctx, -1)
			if e != nil {
				return f, e
			}
			found := false
			for _, p := range ps.Profiles {
				if p.Reference == q.ProfileReference {
					f.Profile = p.ID
					found = true
				}
			}
			if !found {
				return f, ErrInput
			}
		}
		if q.AccountReference != "" {
			accounts, e := r.service.Accounts(ctx)
			if e != nil {
				return f, domainError(ctx, e)
			}
			found := false
			for _, a := range accounts {
				if "account:"+hash(a.ID) == q.AccountReference {
					f.Account = a.ID
					found = true
				}
			}
			if !found {
				return f, ErrInput
			}
		}
		if len(q.KeyHashes) > 0 {
			for _, h := range q.KeyHashes {
				if !validHash(h) {
					return f, ErrInput
				}
			}
			f.APIKey = "keys:" + strings.Join(q.KeyHashes, ",")
		}
	}
	if f.Page < 0 || f.Page > 1000000 || f.From < 0 || f.To < 0 || f.To > 0 && f.From >= f.To || len(f.Profile) > 256 || len(f.Account) > 512 || len(f.Model) > 256 || len(f.APIKey) > 6600 || len(f.ValuationRule) > 128 || f.Revision < 0 || f.Through < 0 || f.ObservationsThrough < 0 || f.TokenEvidenceThrough < 0 || f.ForecastsThrough < 0 || f.VerificationsThrough < 0 {
		return f, ErrInput
	}
	if f.At <= 0 || f.At > r.meta.Boundary.HMS || f.Through > r.meta.Boundary.EventsThrough || f.ObservationsThrough > r.meta.Boundary.ObservationsThrough || f.TokenEvidenceThrough > r.meta.TokenEvidence || f.ForecastsThrough > r.meta.Forecasts || f.VerificationsThrough > r.meta.Verifications {
		return f, ErrBoundary
	}
	if f.ValuationRule == "" {
		return f, ErrInput
	}
	if f.Estimator == "" {
		f.Estimator = cpamp.EstimatorV2
	}
	if f.Estimator != cpamp.EstimatorV2 && f.Estimator != cpamp.EstimatorVersion {
		return f, ErrInput
	}
	// Direct legacy pins may contain exact lineage sets or the unattributed key.
	if f.APIKey != "" && f.APIKey != cpamp.UnknownAPIKey {
		for _, h := range strings.Split(strings.TrimPrefix(f.APIKey, "keys:"), ",") {
			if !validHash(h) {
				return f, ErrInput
			}
		}
	}
	book, e := r.service.Book(ctx, f.ValuationRule)
	if e != nil {
		if ctx.Err() != nil {
			return f, ctx.Err()
		}
		return f, ErrInput
	}
	if book.Policy != "profile-api-value-v2" && book.Policy != "profile-api-value-v3" {
		return f, ErrSource
	}
	if h := book.Historical; h != nil && (h.At > r.meta.Boundary.HMS || h.EventsThrough > r.meta.Boundary.EventsThrough || h.ObservationsThrough > r.meta.Boundary.ObservationsThrough) {
		return f, ErrBoundary
	}
	return f, nil
}

func (r *Reader) Report(ctx context.Context, q Query) (Workspace, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	f, e := r.filter(ctx, q)
	if e != nil {
		return Workspace{}, e
	}
	out, e := r.service.Workspace(ctx, f)
	return out, domainError(ctx, e)
}

// Evidence is the active Compare window proof, including measured bounds,
// selected/shared/boundary rows, canonical closure and historical provenance.
func (r *Reader) Evidence(ctx context.Context, q Query, window string) (Evidence, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	var legacy *EvidencePin
	if q.EvidencePin != nil {
		legacy = q.EvidencePin
		if q.Pin != nil || q.ProfileReference != "" || q.AccountReference != "" || len(q.KeyHashes) != 0 || q.Model != "" || q.From != 0 || q.To != 0 || q.RequestPage != 0 || window != "" && window != legacy.WindowID {
			return Evidence{}, ErrInput
		}
		if len(legacy.SourcePlan) > 256 || len(legacy.TargetPlan) > 256 || len(legacy.Basis) > 64 || len(legacy.Period) > 64 || legacy.Kind != "" && legacy.Kind != "weekly" && legacy.Kind != "5h" || math.IsNaN(legacy.CapacityRatio) || math.IsInf(legacy.CapacityRatio, 0) {
			return Evidence{}, ErrInput
		}
		window = legacy.WindowID
		p := Pin{ProfileFilter: legacy.ProfileFilter}
		q = Query{Pin: &p}
	}
	if window == "" || len(window) > 256 {
		return Evidence{}, ErrInput
	}
	f, e := r.filter(ctx, q)
	if e != nil {
		return Evidence{}, e
	}
	full := cpamp.QuotaUsageFilter{ProfileFilter: f.ProfileFilter, WindowID: window, Period: "history"}
	if legacy != nil {
		full = *legacy
		full.ProfileFilter = f.ProfileFilter
	}
	out, e := r.service.QuotaEvidence(ctx, full)
	return out, domainError(ctx, e)
}
