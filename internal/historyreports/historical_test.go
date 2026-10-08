package historyreports

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
)

func historicalGolden(t *testing.T, name string) []byte {
	t.Helper()
	base := "testdata/historical-golden/"
	f, e := os.Open(base + name + ".gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	b, e := io.ReadAll(z)
	if e != nil {
		t.Fatal(e)
	}
	p, e := os.ReadFile(base + "provenance.json")
	if e != nil {
		t.Fatal(e)
	}
	var m struct {
		Source  string `json:"source_commit"`
		Outputs map[string]string
	}
	if json.Unmarshal(p, &m) != nil || m.Source != "e8492c245bf69144afdcf20b8b4ea8935f0476e5" || m.Outputs[name] != hash(string(b)) {
		t.Fatal("original artifact provenance", name)
	}
	return b
}

type historicalCase struct {
	Name, View, Window, Kind string
	Filter                   LegacyPin
	Quota                    *EvidencePin
	Output                   json.RawMessage
}

func historicalCases(t *testing.T) []historicalCase {
	t.Helper()
	var cases []historicalCase
	if e := json.Unmarshal(historicalGolden(t, "historical-oracle.json"), &cases); e != nil {
		t.Fatal(e)
	}
	return cases
}
func historicalFixture(t *testing.T) *Reader {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sqlite")
	db, e := sql.Open("sqlite3", source)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(historicalGolden(t, "historical-fixture.sql"))); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO settings VALUES('source-private','synthetic-source-private-sentinel',1)`); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE analytics_events SET payload=json_set(payload,'$.private_transport','synthetic-source-private-sentinel');UPDATE analytics_forecasts SET payload=json_set(payload,'$.private_transport','synthetic-source-private-sentinel')`); e != nil {
		t.Fatal(e)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(source, 0400); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	boundary := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hash(string(raw)), LineageSHA256: strings.Repeat("b", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790812800000, EventsThrough: 240, ObservationsThrough: 65}
	s, e := history.Open(filepath.Join(dir, "archive.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	progress, e := s.Import(ctx, history.ImportOptions{SourcePath: source, Boundary: boundary, BatchSize: 13})
	if e != nil || !progress.Complete {
		t.Fatal(progress, e)
	}
	r, e := Prepare(ctx, s, dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Original legacy CSV uses Go-map iteration for price/group metadata rows.
// Compare header and complete encoded row bytes as a multiset for only those
// exports. No cell/JSON value, numeric formatting or escaping is normalized.
func csvRowBytes(t *testing.T, b []byte) []string {
	t.Helper()
	rows, e := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var encoded bytes.Buffer
		w := csv.NewWriter(&encoded)
		if e = w.Write(row); e != nil {
			t.Fatal(e)
		}
		w.Flush()
		out = append(out, encoded.String())
	}
	if len(out) > 1 {
		sort.Strings(out[1:])
	}
	return out
}

func TestHistoricalOriginalDomainAndControllerParity(t *testing.T) {
	r := historicalFixture(t)
	ctx := context.Background()
	var exports map[string]string
	if e := json.Unmarshal(historicalGolden(t, "historical-exports.json"), &exports); e != nil {
		t.Fatal(e)
	}
	for _, tc := range historicalCases(t) {
		t.Run(tc.View+"/"+tc.Name, func(t *testing.T) {
			q := LegacyQuery{Pin: &tc.Filter}
			opts := LegacyExportOptions{View: tc.View, WindowID: tc.Window, Kind: tc.Kind}
			var value any
			var err error
			switch tc.View {
			case "profiles":
				value, err = r.ProfileReport(ctx, q)
			case "profile-proof":
				value, err = r.ProfileProof(ctx, q, tc.Window)
			case "history":
				value, err = r.HistoryReport(ctx, q, tc.Kind)
			case "history-proof":
				value, err = r.HistoryProof(ctx, q, tc.Window)
			case "quota":
				value, err = r.QuotaUsage(ctx, QuotaQuery{Pin: tc.Quota})
			case "forecasts":
				var p ForecastPage
				p, err = r.Forecasts(ctx, ForecastQuery{Selection: q, WindowID: tc.Window, Limit: 100})
				value = p.Forecasts
			default:
				t.Fatal(tc.View)
			}
			if err != nil {
				t.Fatal(err)
			}
			exactJSON(t, tc.View, value, tc.Output)
			if tc.View == "forecasts" {
				return
			}
			for _, format := range []string{"json", "csv"} {
				var b bytes.Buffer
				if tc.View == "quota" {
					if format == "json" {
						err = r.ExportQuotaJSON(ctx, QuotaQuery{Pin: tc.Quota}, &b)
					} else {
						err = r.ExportQuotaCSV(ctx, QuotaQuery{Pin: tc.Quota}, &b)
					}
				} else if format == "json" {
					err = r.ExportLegacyJSON(ctx, q, opts, &b)
				} else {
					err = r.ExportLegacyCSV(ctx, q, opts, &b)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := exports[tc.View+"/"+tc.Name+"/"+format]
				if format == "csv" && (tc.View == "profiles" || tc.View == "profile-proof" || tc.View == "history-proof") {
					if !reflect.DeepEqual(csvRowBytes(t, b.Bytes()), csvRowBytes(t, []byte(want))) {
						t.Fatal("original CSV row bytes differ")
					}
				} else if b.String() != want {
					t.Fatal("original export bytes differ", format)
				}
				if strings.Contains(b.String(), "synthetic-source-private-sentinel") {
					t.Fatal("source-private value exposed")
				}
			}
		})
	}
}

func TestHistoricalForecastBoundsAndScenarioSemantics(t *testing.T) {
	r := historicalFixture(t)
	ctx := context.Background()
	legacy, e := r.ProfileReport(ctx, LegacyQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if legacy.Totals.Requests != 80 || legacy.Filter.At != r.meta.Boundary.HMS || legacy.Filter.Through != 240 || legacy.Filter.ValuationRule != "" {
		t.Fatal("default report checkpoint", legacy.Filter, legacy.Totals.Requests)
	}
	for _, w := range legacy.Windows {
		if w.Account == "forecast" && (w.Comparison == nil || w.Comparison.ActualCycle == nil) {
			t.Fatal("v3 estimator not extracted")
		}
	}
	var v2 LegacyProfileReport
	for _, tc := range historicalCases(t) {
		if tc.View == "profiles" && tc.Name == "v2" {
			v2, e = r.ProfileReport(ctx, LegacyQuery{Pin: &tc.Filter})
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	for _, w := range v2.Windows {
		if w.Comparison != nil && (w.Comparison.EstimatorID != "quota-estimator-v2" || w.Comparison.ActualCycle != nil) {
			t.Fatal("v2 replay upgraded")
		}
	}
	proof, e := r.ProfileProof(ctx, LegacyQuery{}, "forecast-window")
	if e != nil {
		t.Fatal(e)
	}
	if len(proof.Forecasts) != 2 || len(proof.Forecasts[0].Results) != 1 || len(proof.Forecasts[1].Results) != 2 {
		t.Fatal("future ledger entry escaped pin", proof.Forecasts)
	}
	if proof.Forecasts[0].Results[0].State != "verified" || proof.Forecasts[1].Results[1].State != "unscorable" {
		t.Fatal("verification revision rewritten")
	}
	if proof.Window.Comparison.LastVerification != nil {
		t.Fatal("stale verified badge resurrected")
	}
	missing, e := r.ProfileProof(ctx, LegacyQuery{}, "missing-window")
	if e != nil || missing.Window.Comparison.Unpriced != 1 || missing.Window.Comparison.Capacity != nil {
		t.Fatal("missing price silently calibrated", missing.Window.Comparison, e)
	}
	for _, gateway := range []bool{false, true} {
		usage, e := r.QuotaUsage(ctx, QuotaQuery{Period: "history", Selection: Query{KeyHashes: []string{strings.Repeat("a", 64)}}, GatewayOnly: gateway})
		if e != nil {
			t.Fatal(e)
		}
		for _, w := range usage.Windows {
			if w.ID == "forecast-window" {
				if !gateway && w.Attribution.Status != "unavailable" || gateway && (w.Attribution.Status != "estimate" || w.Attribution.Percent == nil || *w.Attribution.Percent != 20) {
					t.Fatal("outside-use assumption lost", w.Attribution)
				}
			}
		}
	}
	q := ForecastQuery{WindowID: "forecast-window", Limit: 1}
	first, e := r.Forecasts(ctx, q)
	if e != nil || first.Total != 2 || len(first.Forecasts) != 1 || first.NextCursor == "" {
		t.Fatal(first, e)
	}
	path := r.ArtifactPath()
	reopened, e := Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	q.Cursor = first.NextCursor
	second, e := reopened.Forecasts(ctx, q)
	if e != nil || len(second.Forecasts) != 1 || second.Forecasts[0].Forecast.ID == first.Forecasts[0].Forecast.ID || second.NextCursor != "" {
		t.Fatal(second, e)
	}
	q.Limit = 2
	if _, e = r.Forecasts(ctx, q); !errors.Is(e, ErrCursor) {
		t.Fatal("cursor filters not bound", e)
	}
	q.Limit = 1
	q.Cursor += "x"
	if _, e = r.Forecasts(ctx, q); !errors.Is(e, ErrCursor) {
		t.Fatal("cursor tampering", e)
	}
	scenario, e := r.QuotaUsage(ctx, QuotaQuery{Period: "history", WindowID: "a-h", SourcePlan: "claude-max-5", TargetPlan: "claude-max-20", CapacityRatio: 3})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, w := range scenario.Windows {
		if w.ID == "a-h" {
			for _, c := range w.Comparisons {
				if c.Tier.ID == "claude-max-20" {
					found = c.Status == "scenario" && c.Ratio != nil && *c.Ratio == 3 && c.Percent != nil
				}
			}
		}
	}
	if !found {
		t.Fatal("explicit subscription scenario missing")
	}
	raw, e := r.HistoryReport(ctx, LegacyQuery{}, "")
	if e != nil || raw.Through != 240 || raw.ObservationThrough != 65 || raw.Generated != r.meta.Boundary.HMS {
		t.Fatal(raw, e)
	}
	for _, w := range raw.Windows {
		if w.Tokens.Total > 100000000 {
			t.Fatal("future source cursor read", w)
		}
	}
}

func TestHistoricalOfflineDefaultsCacheAndCompatibility(t *testing.T) {
	r := historicalFixture(t)
	ctx := context.Background()
	f, e := r.legacyFilter(ctx, LegacyQuery{})
	if e != nil {
		t.Fatal(e)
	}
	var count int
	if e = r.db.QueryRow(`SELECT count(*) FROM pacer_historical_reports`).Scan(&count); e != nil || count != 4 {
		t.Fatal("offline defaults missing", count, e)
	}
	key := historicalKey("profiles", f, "")
	value, e := historicalRead(ctx, r, key, func() (LegacyProfileReport, error) {
		t.Fatal("first default query recalculated archive")
		return LegacyProfileReport{}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	value.Config.Profiles[0].Name = "caller mutation"
	value.Normalization.Prices = nil
	again, e := r.ProfileReport(ctx, LegacyQuery{})
	if e != nil || again.Config.Profiles[0].Name == "caller mutation" || len(again.Normalization.Prices) == 0 {
		t.Fatal("mutable report cache", e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := r.ProfileProof(ctx, LegacyQuery{}, "forecast-window")
			if e == nil && len(p.Forecasts) != 2 {
				e = ErrSource
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for _, change := range []func(*LegacyPin){func(p *LegacyPin) { p.At++ }, func(p *LegacyPin) { p.Through++ }, func(p *LegacyPin) { p.ObservationsThrough++ }, func(p *LegacyPin) { p.ForecastsThrough++ }, func(p *LegacyPin) { p.VerificationsThrough++ }} {
		pin := f
		change(&pin)
		if _, e = r.ProfileReport(ctx, LegacyQuery{Pin: &pin}); !errors.Is(e, ErrBoundary) {
			t.Fatal("out-of-bound pin accepted", e)
		}
	}
	if _, e = r.ProfileReport(ctx, LegacyQuery{Pin: &f, Selection: Query{Model: "test"}}); !errors.Is(e, ErrInput) {
		t.Fatal("ambiguous filter", e)
	}
	if _, e = r.HistoryReport(ctx, LegacyQuery{Selection: Query{Model: "test"}}, ""); !errors.Is(e, ErrInput) {
		t.Fatal("ignored model filter", e)
	}
	if _, e = r.QuotaUsage(ctx, QuotaQuery{Period: "anything"}); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	for _, q := range []QuotaQuery{{CapacityRatio: 2}, {WindowID: "a-h", SourcePlan: "unknown"}, {WindowID: "a-h", CapacityRatio: 2, TargetPlan: "unknown"}, {WindowID: "a-h", CapacityRatio: 10001}} {
		if _, e = r.QuotaUsage(ctx, q); !errors.Is(e, ErrInput) {
			t.Fatal("invalid scenario accepted", q, e)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = r.ProfileReport(cancelled, LegacyQuery{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = r.db.Exec(`DELETE FROM analytics_forecasts`); e == nil {
		t.Fatal("read API made artifact writable")
	}
	// A previous-generation artifact remains valid for the stable API, but does
	// not initialize/write a legacy price book while serving a new read endpoint.
	r.meta.HistoricalReads = false
	if _, e = r.ProfileReport(ctx, LegacyQuery{}); !errors.Is(e, ErrNotPrepared) {
		t.Fatal(e)
	}
	if _, e = r.Report(ctx, Query{}); e != nil {
		t.Fatal("stable workspace contract changed", e)
	}
}

type failedExportWriter struct{ err error }

func (w failedExportWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHistoricalExportsStableOrderAndWriterErrors(t *testing.T) {
	r := historicalFixture(t)
	ctx := context.Background()
	failure := errors.New("synthetic writer failure")
	for _, view := range []string{"profiles", "profile-proof", "history", "history-proof"} {
		opts := LegacyExportOptions{View: view}
		if view == "profile-proof" || view == "history-proof" {
			opts.WindowID = "a-week"
		}
		var first bytes.Buffer
		if e := r.ExportLegacyCSV(ctx, LegacyQuery{}, opts, &first); e != nil {
			t.Fatal(e)
		}
		for range 3 {
			var next bytes.Buffer
			if e := r.ExportLegacyCSV(ctx, LegacyQuery{}, opts, &next); e != nil || !bytes.Equal(first.Bytes(), next.Bytes()) {
				t.Fatal("unstable native CSV", view, e)
			}
		}
		if e := r.ExportLegacyCSV(ctx, LegacyQuery{}, opts, failedExportWriter{failure}); !errors.Is(e, failure) {
			t.Fatal("CSV writer failure lost", e)
		}
		if e := r.ExportLegacyJSON(ctx, LegacyQuery{}, opts, failedExportWriter{failure}); !errors.Is(e, failure) {
			t.Fatal("JSON writer failure lost", e)
		}
	}
	if e := r.ExportQuotaCSV(ctx, QuotaQuery{}, failedExportWriter{failure}); !errors.Is(e, failure) {
		t.Fatal(e)
	}
	if e := r.ExportQuotaJSON(ctx, QuotaQuery{}, failedExportWriter{failure}); !errors.Is(e, failure) {
		t.Fatal(e)
	}
}
