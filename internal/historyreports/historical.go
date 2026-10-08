package historyreports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

// These source DTOs are management-private, never inference-key usage responses.
type LegacyPin = cpamp.ProfileFilter
type LegacyProfileReport = cpamp.ProfileReport
type LegacyProfileProof = cpamp.ProfileProof
type LegacyHistoryReport = cpamp.Report
type LegacyHistoryProof = cpamp.Proof
type QuotaReport = cpamp.QuotaUsageReport
type Forecast = cpamp.ForecastEvidence

var ErrNotPrepared = errors.New("history reports: historical read preparation required")

// HistoricalReady is a metadata-only capability check for the prepared artifact.
func (r *Reader) HistoricalReady() bool { return r.meta.HistoricalReads }

// LegacyQuery accepts either a complete source pin (including empty valuation
// rule on older reports) or the existing native selection/reference shape.
// These older views retain captured-price/estimator semantics; a v3 valuation
// rule in a source pin does not turn them into ProfileWorkspace calculations.
type LegacyQuery struct {
	Pin       *LegacyPin `json:"pin,omitempty"`
	Selection Query      `json:"selection,omitempty"`
}

func emptyQuery(q Query) bool {
	return q.Pin == nil && q.EvidencePin == nil && q.ProfileReference == "" && q.AccountReference == "" && len(q.KeyHashes) == 0 && q.Model == "" && q.From == 0 && q.To == 0 && q.RequestPage == 0
}
func (r *Reader) legacyFilter(ctx context.Context, q LegacyQuery) (LegacyPin, error) {
	if !r.meta.HistoricalReads {
		return LegacyPin{}, ErrNotPrepared
	}
	if q.Pin != nil && !emptyQuery(q.Selection) {
		return LegacyPin{}, ErrInput
	}
	var input Query
	rule := ""
	if q.Pin != nil {
		pin := Pin{ProfileFilter: *q.Pin}
		rule = pin.ValuationRule
		// Old views did not require a comparison book. Reuse all frozen-bound
		// validations without adding that book to the returned legacy pin.
		if pin.ValuationRule == "" {
			pin.ValuationRule = r.meta.Pin.ValuationRule
		}
		input = Query{Pin: &pin}
	} else {
		input = q.Selection
		if input.Pin != nil || input.EvidencePin != nil {
			return LegacyPin{}, ErrInput
		}
	}
	f, err := r.filter(ctx, input)
	if err != nil {
		return LegacyPin{}, err
	}
	if f.Page != 0 {
		return LegacyPin{}, ErrInput
	}
	f.ValuationRule = rule
	return f.ProfileFilter, nil
}

func historicalKey(view string, filter any, scope string) string {
	b, _ := json.Marshal(struct {
		View   string
		Filter any
		Scope  string
	}{view, filter, scope})
	return hash(string(b))
}

// Historical defaults are materialized offline. Filtered cold queries retain
// the source algorithm and cooperative context checks, not a hard CPU deadline.
// The separate byte-bounded cache never exposes shared mutable result objects.
func historicalRead[T any](ctx context.Context, r *Reader, key string, compute func() (T, error)) (T, error) {
	var zero T
	select {
	case r.historicalGate <- struct{}{}:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	defer func() { <-r.historicalGate }()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	r.historicalClock++
	b, ok := r.historicalCache[key]
	if !ok {
		err := r.db.QueryRowContext(ctx, `SELECT payload FROM pacer_historical_reports WHERE cache_key=?`, key).Scan(&b.payload)
		if errors.Is(err, sql.ErrNoRows) {
			result, e := compute()
			if e != nil {
				return zero, domainError(ctx, e)
			}
			b.payload, err = json.Marshal(result)
		}
		if err != nil {
			return zero, domainError(ctx, err)
		}
		const maxBytes = 32 << 20
		if len(b.payload) <= maxBytes {
			for len(r.historicalCache) >= 16 || r.historicalBytes+len(b.payload) > maxBytes {
				old := ""
				var age uint64
				for k, v := range r.historicalCache {
					if old == "" || v.used < age {
						old, age = k, v.used
					}
				}
				r.historicalBytes -= len(r.historicalCache[old].payload)
				delete(r.historicalCache, old)
			}
			r.historicalBytes += len(b.payload)
			b.used = r.historicalClock
			r.historicalCache[key] = b
		}
	} else {
		b.used = r.historicalClock
		r.historicalCache[key] = b
	}
	var out T
	if err := json.Unmarshal(b.payload, &out); err != nil {
		return zero, ErrSource
	}
	return out, nil
}

type historicalEntry struct {
	payload []byte
	used    uint64
}

func (r *Reader) ProfileReport(ctx context.Context, q LegacyQuery) (LegacyProfileReport, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	f, e := r.legacyFilter(ctx, q)
	if e != nil {
		return LegacyProfileReport{}, e
	}
	return historicalRead(ctx, r, historicalKey("profiles", f, ""), func() (LegacyProfileReport, error) { return r.service.ProfileReport(ctx, f) })
}
func (r *Reader) ProfileProof(ctx context.Context, q LegacyQuery, window string) (LegacyProfileProof, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	if window == "" || len(window) > 256 {
		return LegacyProfileProof{}, ErrInput
	}
	f, e := r.legacyFilter(ctx, q)
	if e != nil {
		return LegacyProfileProof{}, e
	}
	return historicalRead(ctx, r, historicalKey("profile-proof", f, window), func() (LegacyProfileProof, error) { return r.service.ProfileProof(ctx, window, f) })
}
func (r *Reader) HistoryReport(ctx context.Context, q LegacyQuery, kind string) (LegacyHistoryReport, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	if len(kind) > 64 {
		return LegacyHistoryReport{}, ErrInput
	}
	f, e := r.legacyFilter(ctx, q)
	if e != nil {
		return LegacyHistoryReport{}, e
	}
	if f.Profile != "" || f.Model != "" {
		return LegacyHistoryReport{}, ErrInput
	}
	return historicalRead(ctx, r, historicalKey("history", f, kind), func() (LegacyHistoryReport, error) { return r.service.HistoryAt(ctx, f, kind) })
}
func (r *Reader) HistoryProof(ctx context.Context, q LegacyQuery, window string) (LegacyHistoryProof, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	if window == "" || len(window) > 256 {
		return LegacyHistoryProof{}, ErrInput
	}
	f, e := r.legacyFilter(ctx, q)
	if e != nil {
		return LegacyHistoryProof{}, e
	}
	if f.Profile != "" || f.Model != "" || f.From != 0 || f.To != 0 || f.Account != "" {
		return LegacyHistoryProof{}, ErrInput
	}
	return historicalRead(ctx, r, historicalKey("history-proof", f, window), func() (LegacyHistoryProof, error) {
		return r.service.ProofAt(ctx, window, f.Through, f.ObservationsThrough, f.At, f.APIKey)
	})
}

type QuotaQuery struct {
	Pin           *EvidencePin `json:"pin,omitempty"`
	Selection     Query        `json:"selection,omitempty"`
	Kind          string       `json:"kind,omitempty"`
	Period        string       `json:"period,omitempty"`
	WindowID      string       `json:"window,omitempty"`
	Basis         string       `json:"basis,omitempty"`
	SourcePlan    string       `json:"source_plan,omitempty"`
	TargetPlan    string       `json:"target_plan,omitempty"`
	CapacityRatio float64      `json:"capacity_ratio,omitempty"`
	GatewayOnly   bool         `json:"gateway_only,omitempty"`
}

func (r *Reader) quotaFilter(ctx context.Context, q QuotaQuery) (EvidencePin, error) {
	f := EvidencePin{Kind: q.Kind, Period: q.Period, WindowID: q.WindowID, Basis: q.Basis, SourcePlan: q.SourcePlan, TargetPlan: q.TargetPlan, CapacityRatio: q.CapacityRatio, GatewayOnly: q.GatewayOnly}
	selection := LegacyQuery{Selection: q.Selection}
	if q.Pin != nil {
		if !emptyQuery(q.Selection) || q.Kind != "" || q.Period != "" || q.WindowID != "" || q.Basis != "" || q.SourcePlan != "" || q.TargetPlan != "" || q.CapacityRatio != 0 || q.GatewayOnly {
			return f, ErrInput
		}
		f = *q.Pin
		selection = LegacyQuery{Pin: &f.ProfileFilter}
	}
	p, e := r.legacyFilter(ctx, selection)
	if e != nil {
		return f, e
	}
	f.ProfileFilter = p
	if f.Period == "" {
		f.Period = "current"
	}
	if f.Basis == "" {
		f.Basis = "today"
	}
	if (f.Kind != "" && f.Kind != "weekly" && f.Kind != "5h") || (f.Period != "current" && f.Period != "history") || (f.Basis != "then" && f.Basis != "today") || len(f.WindowID) > 256 || len(f.SourcePlan) > 64 || len(f.TargetPlan) > 64 || math.IsNaN(f.CapacityRatio) || math.IsInf(f.CapacityRatio, 0) || f.CapacityRatio < 0 || f.CapacityRatio > 10000 {
		return f, ErrInput
	}
	if !cpamp.ValidQuotaScenario(f) {
		return f, ErrInput
	}
	return f, nil
}
func (r *Reader) QuotaUsage(ctx context.Context, q QuotaQuery) (QuotaReport, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	f, e := r.quotaFilter(ctx, q)
	if e != nil {
		return QuotaReport{}, e
	}
	return historicalRead(ctx, r, historicalKey("quota", f, ""), func() (QuotaReport, error) { return r.service.QuotaUsage(ctx, f) })
}

type ForecastQuery struct {
	Selection LegacyQuery `json:"selection"`
	WindowID  string      `json:"window"`
	Limit     int         `json:"limit"`
	Cursor    string      `json:"cursor,omitempty"`
}
type ForecastPage struct {
	Pin        LegacyPin  `json:"pin"`
	Forecasts  []Forecast `json:"forecasts"`
	Total      int        `json:"total"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func (r *Reader) Forecasts(ctx context.Context, q ForecastQuery) (ForecastPage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	if q.Limit == 0 {
		q.Limit = 25
	}
	if q.Limit < 1 || q.Limit > 100 || q.WindowID == "" || len(q.WindowID) > 256 {
		return ForecastPage{}, ErrInput
	}
	f, e := r.legacyFilter(ctx, q.Selection)
	if e != nil {
		return ForecastPage{}, e
	}
	// Ledger is full source-account/window evidence, as in ProfileProof, rather
	// than key/profile-attributed spend. Refuse ignored selection filters.
	if f.Profile != "" || f.Account != "" || f.Model != "" || f.APIKey != "" || f.From != 0 || f.To != 0 {
		return ForecastPage{}, ErrInput
	}
	binding := historicalKey("forecast-page", struct {
		Pin    LegacyPin
		Window string
		Limit  int
	}{f, q.WindowID, q.Limit}, r.meta.Boundary.SnapshotSHA256)
	var after pageCursor
	if q.Cursor != "" {
		after, e = r.parseCursor(q.Cursor, binding)
		if e != nil {
			return ForecastPage{}, e
		}
	}
	items, e := historicalRead(ctx, r, historicalKey("forecasts", f, q.WindowID), func() ([]Forecast, error) { return r.service.ForecastLedger(ctx, q.WindowID, f) })
	if e != nil {
		return ForecastPage{}, e
	}
	start := 0
	if q.Cursor != "" {
		found := false
		for i, item := range items {
			if item.Forecast.ID == after.ID && item.Forecast.Issued == after.Reset {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return ForecastPage{}, ErrCursor
		}
	}
	end := min(start+q.Limit, len(items))
	out := ForecastPage{Pin: f, Forecasts: append([]Forecast{}, items[start:end]...), Total: len(items)}
	if end < len(items) {
		last := items[end-1].Forecast
		out.NextCursor = r.cursor(pageCursor{Binding: binding, ID: last.ID, Reset: last.Issued})
	}
	return out, nil
}

// All expensive default historical aggregates are computed during explicit
// offline preparation, not during the first management request. No background
// worker, source update or new forecast is run.
func prepareHistoricalReads(ctx context.Context, db *sql.DB, s *cpamp.Service, m prepared) error {
	if err := s.PrepareLegacy(ctx); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE pacer_historical_reports(cache_key TEXT PRIMARY KEY,payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS report_forecast_window ON analytics_forecasts(window_id,issued_ms);
CREATE INDEX IF NOT EXISTS report_result_forecast ON analytics_forecast_results(forecast_id,settled_ms);`); err != nil {
		return err
	}
	f := m.Pin.ProfileFilter
	f.ValuationRule = ""
	put := func(view string, filter any, value any) error {
		b, e := json.Marshal(value)
		if e != nil {
			return e
		}
		_, e = db.ExecContext(ctx, `INSERT INTO pacer_historical_reports VALUES(?,?)`, historicalKey(view, filter, ""), b)
		return e
	}
	profile, e := s.ProfileReport(ctx, f)
	if e != nil {
		return e
	}
	if e = put("profiles", f, profile); e != nil {
		return e
	}
	history, e := s.HistoryAt(ctx, f, "")
	if e != nil {
		return e
	}
	if e = put("history", f, history); e != nil {
		return e
	}
	for _, period := range []string{"current", "history"} {
		q := EvidencePin{ProfileFilter: f, Period: period, Basis: "today"}
		report, e := s.QuotaUsage(ctx, q)
		if e != nil {
			return e
		}
		if e = put("quota", q, report); e != nil {
			return e
		}
	}
	return nil
}
