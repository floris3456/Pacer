package cpamp

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/apikeyalias"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/model"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/modelprice"
)

// Service has only offline/read capabilities. None of CPAMP's collector,
// provider access, HTTP application, scheduler or background workers is linked.
type Service struct {
	db            *sql.DB
	store         readStore
	at            int64
	PollInterval  time.Duration
	ready         atomic.Bool
	historyWork   chan struct{}
	workspaceGate chan struct{}
	cache         map[string]cachedReport
	cacheClock    uint64
	reportMu      sync.Mutex
	reportKey     string
	reportCache   Report
	bounds        ProfileFilter
	facts         atomic.Pointer[QuotaFacts]
}

// UseQuotaFacts lets Workspace rebuild eligible reports from prepared facts.
func (s *Service) UseQuotaFacts(q *QuotaFacts) { s.facts.Store(q) }

// WorkspaceOriginal always runs the original builder (oracle for tests).
func (s *Service) WorkspaceOriginal(ctx context.Context, f ProfileWorkspaceFilter) (ProfileWorkspaceReport, error) {
	return s.buildProfileWorkspace(ctx, f, false)
}

type cachedReport struct {
	payload        []byte
	used           uint64
	nextTransition int64
}
type readStore struct{ db *sql.DB }

func (r readStore) LoadModelPrices(ctx context.Context) (map[string]model.ModelPrice, error) {
	return modelprice.Load(ctx, r.db)
}
func (r readStore) LoadAPIKeyAliases(ctx context.Context) ([]model.APIKeyAlias, error) {
	return apikeyalias.Load(ctx, r.db)
}
func New(db *sql.DB, at int64) *Service {
	s := &Service{db: db, store: readStore{db}, at: at, PollInterval: 5 * time.Minute, historyWork: make(chan struct{}, 1), workspaceGate: make(chan struct{}, 1), cache: map[string]cachedReport{}}
	s.ready.Store(true)
	return s
}
func (s *Service) acquireHistory(ctx context.Context) error {
	select {
	case s.historyWork <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) releaseHistory() { <-s.historyWork }
func (s *Service) wakeWorkspace()  {} // Preparation has no background worker.
func (s *Service) PrepareBook(ctx context.Context, cp HistoricalImportCheckpoint) (ComparisonPriceBook, error) {
	if cp.At >= HistoricalValuationCutoff {
		if _, e := s.PinHistoricalValuation(ctx, cp); e != nil {
			return ComparisonPriceBook{}, e
		}
	} else {
		// A source snapshot can retain a later active cohort than this import's
		// explicit H. Before the cutoff the new book has no assumed-write cohort;
		// retain all older books/cohort rows for exact saved-pin replay.
		if _, e := s.db.ExecContext(ctx, `DELETE FROM analytics_state WHERE name='historical_valuation'`); e != nil {
			return ComparisonPriceBook{}, e
		}
	}
	return s.comparisonPriceBook(ctx, "")
}
func (s *Service) Book(ctx context.Context, id string) (ComparisonPriceBook, error) {
	return s.comparisonPriceBook(ctx, id)
}

// PrepareLegacy performs the legacy price-book initialization only during the
// explicit offline preparation step. Reads of old prepared artifacts fail rather
// than trying to write through a read-only connection.
func (s *Service) PrepareLegacy(ctx context.Context) error {
	_, err := s.normalizationRules(ctx)
	return err
}
func (s *Service) HistoryAt(ctx context.Context, f ProfileFilter, kind string) (Report, error) {
	reader := New(s.db, f.At)
	reader.PollInterval, reader.bounds = s.PollInterval, f
	return reader.Report(ctx, f.Account, kind, f.From, f.To, f.APIKey)
}
func (s *Service) ForecastLedger(ctx context.Context, window string, f ProfileFilter) ([]ForecastEvidence, error) {
	return s.forecastEvidence(ctx, window, f)
}

// Same input-only scenario checks as QuotaUsage, before expensive derivation.
func ValidQuotaScenario(f QuotaUsageFilter) bool {
	if !validCapacityRatio(f.CapacityRatio) {
		return false
	}
	if f.CapacityRatio > 0 {
		if _, ok := subscriptionTier(f.TargetPlan); !ok || f.WindowID == "" {
			return false
		}
	}
	if f.SourcePlan != "" {
		if _, ok := subscriptionTier(f.SourcePlan); !ok || f.WindowID == "" {
			return false
		}
	}
	return true
}
func (s *Service) Workspace(ctx context.Context, f ProfileWorkspaceFilter) (ProfileWorkspaceReport, error) {
	// Frozen pins need no worker/expiry refresh. Cache bytes rather than shared
	// mutable DTOs; each caller owns its maps/slices. The same 16-entry bound and
	// cold/warm cache metadata as CPAMP preserve export envelope compatibility.
	select {
	case s.workspaceGate <- struct{}{}:
	case <-ctx.Done():
		return ProfileWorkspaceReport{}, ctx.Err()
	}
	defer func() { <-s.workspaceGate }()
	if err := ctx.Err(); err != nil {
		return ProfileWorkspaceReport{}, err
	}
	page := f.Page
	f.Page = 0
	key := string(encode(f))
	s.cacheClock++
	var r ProfileWorkspaceReport
	var e error
	if entry, ok := s.cache[key]; ok {
		e = json.Unmarshal(entry.payload, &r)
		if e != nil {
			return r, e
		}
		entry.used = s.cacheClock
		s.cache[key] = entry
		r.Cache = &WorkspaceCacheStatus{Refreshing: false}
		r.NextTransition = entry.nextTransition
	} else {
		fast := false
		if q := s.facts.Load(); q != nil {
			r, fast, e = s.FactsWorkspace(ctx, q, f, FactsOptions{})
		}
		if !fast && e == nil {
			r, e = s.buildProfileWorkspace(ctx, f, false)
		}
		if e != nil {
			return r, e
		}
		if len(s.cache) >= 16 {
			old := ""
			var age uint64
			for k, v := range s.cache {
				if old == "" || v.used < age {
					old = k
					age = v.used
				}
			}
			delete(s.cache, old)
		}
		s.cache[key] = cachedReport{payload: encode(r), used: s.cacheClock, nextTransition: r.NextTransition}
	}
	if page > 0 {
		r.Filter.Page = page
		r.Requests, e = s.workspaceRequests(ctx, r, page)
	}
	return r, e
}
