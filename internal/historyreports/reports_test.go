package historyreports

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
)

type oracleCase struct {
	Name     string                     `json:"name"`
	Filter   Pin                        `json:"filter"`
	Report   json.RawMessage            `json:"report"`
	Evidence map[string]json.RawMessage `json:"evidence"`
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	f, e := os.Open("testdata/golden/" + name + ".gz")
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
	p, e := os.ReadFile("testdata/golden/provenance.json")
	if e != nil {
		t.Fatal(e)
	}
	var provenance struct {
		Commit  string
		Outputs map[string]string
	}
	if json.Unmarshal(p, &provenance) != nil || provenance.Commit != "e8492c245bf69144afdcf20b8b4ea8935f0476e5" {
		t.Fatal("invalid original-source provenance")
	}
	if hash(string(b)) != provenance.Outputs[name] {
		t.Fatal("changed original oracle artifact", name)
	}
	return b
}
func cases(t *testing.T) []oracleCase {
	t.Helper()
	var out []oracleCase
	if e := json.Unmarshal(golden(t, "oracle.json"), &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func frozenFixture(t *testing.T, modify ...func(*sql.DB)) (*history.Store, history.Boundary) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "synthetic-cpamp.sqlite")
	db, e := sql.Open("sqlite3", source)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(golden(t, "fixture.sql"))); e != nil {
		t.Fatal(e)
	}
	// Raw request/config secrets are deliberately absent from report materialization.
	if _, e = db.Exec(`INSERT INTO settings VALUES('secret', 'sk-synthetic-private-material', 1)`); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO usage_events(id,event_hash,timestamp_ms,timestamp,model,raw_json,fail_body,created_at_ms) VALUES(999,'private-row',1,'fixture','fixture','{"api_key":"sk-synthetic-private-material"}','sk-synthetic-private-material',1)`); e != nil {
		t.Fatal(e)
	}
	for _, f := range modify {
		f(db)
	}
	var v3 int
	if e = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='analytics_token_evidence'`).Scan(&v3); e != nil {
		t.Fatal(e)
	}
	schema := "cpamp-usage-v3"
	if v3 == 0 {
		schema = "cpamp-usage-v2"
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
	sum := sha256.Sum256(raw)
	b := history.Boundary{Schema: schema, SnapshotSHA256: hex.EncodeToString(sum[:]), LineageSHA256: strings.Repeat("a", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790812800000, EventsThrough: 40, ObservationsThrough: 18}
	s, e := history.Open(filepath.Join(dir, "archive.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	p, e := s.Import(context.Background(), history.ImportOptions{SourcePath: source, Boundary: b, BatchSize: 7})
	if e != nil || !p.Complete {
		t.Fatal(p, e)
	}
	return s, b
}
func fixtureReader(t *testing.T) *Reader {
	t.Helper()
	s, _ := frozenFixture(t)
	r, e := Prepare(context.Background(), s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func exactJSON(t *testing.T, name string, got any, want json.RawMessage) {
	t.Helper()
	b, e := json.Marshal(got)
	if e != nil {
		t.Fatal(e)
	}
	var compact bytes.Buffer
	if json.Compact(&compact, want) != nil {
		t.Fatal("invalid original JSON")
	}
	if !bytes.Equal(b, compact.Bytes()) {
		// Persist only synthetic difference receipts in test-local scratch.
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "native.json"), b, 0600)
		os.WriteFile(filepath.Join(dir, "original.json"), compact.Bytes(), 0600)
		at := 0
		w := compact.Bytes()
		for at < len(b) && at < len(w) && b[at] == w[at] {
			at++
		}
		lo := max(at-100, 0)
		t.Fatalf("%s differs at byte %d: native=%s original=%s", name, at, b[lo:min(len(b), at+250)], w[lo:min(len(w), at+250)])
	}
}

func TestOriginalCPAMPReportAndExportParity(t *testing.T) {
	verifyOriginalCPAMPParity(t, fixtureReader(t))
}

func verifyOriginalCPAMPParity(t *testing.T, r *Reader) {
	t.Helper()
	ctx := context.Background()
	var exportOracle map[string]string
	if e := json.Unmarshal(golden(t, "exports.json"), &exportOracle); e != nil {
		t.Fatal(e)
	}
	for _, c := range cases(t) {
		t.Run(c.Name, func(t *testing.T) {
			q := Query{Pin: &c.Filter}
			report, e := r.Report(ctx, q)
			if e != nil {
				t.Fatal(e)
			}
			exactJSON(t, "workspace", report, c.Report)
			for _, format := range []string{"daily", "comparisons", "json"} {
				var out bytes.Buffer
				if format == "json" {
					e = r.ExportJSON(ctx, q, ExportOptions{}, &out)
				} else {
					e = r.ExportCSV(ctx, q, ExportOptions{CSV: format}, &out)
				}
				if e != nil {
					t.Fatal(e)
				}
				if out.String() != exportOracle[c.Name+"/"+format] {
					t.Fatalf("%s export differs from ORIGINAL controller bytes", format)
				}
			}
			for id, want := range c.Evidence {
				eq := q
				if c.Name == "v2-evidence-pin" {
					var saved Evidence
					if json.Unmarshal(want, &saved) != nil {
						t.Fatal("original evidence")
					}
					eq = Query{EvidencePin: &saved.Filter}
				}
				evidence, e := r.Evidence(ctx, eq, id)
				if e != nil {
					t.Fatal(id, e)
				}
				exactJSON(t, id, evidence, want)
				for _, format := range []string{"json", "csv"} {
					var out bytes.Buffer
					opts := ExportOptions{WindowID: id}
					if format == "json" {
						e = r.ExportJSON(ctx, eq, opts, &out)
					} else {
						e = r.ExportCSV(ctx, eq, opts, &out)
					}
					if e != nil {
						t.Fatal(e)
					}
					if out.String() != exportOracle[c.Name+"/"+id+"/"+format] {
						t.Fatalf("%s %s export differs from ORIGINAL bytes", id, format)
					}
				}
			}
		})
	}
}

func TestWindowValuationSemanticsAndGaps(t *testing.T) {
	r := fixtureReader(t)
	ctx := context.Background()
	report, e := r.Report(ctx, Query{})
	if e != nil {
		t.Fatal(e)
	}
	if report.Totals.Requests != 40 || report.Totals.Value.Missing != 1 || report.Totals.Value.Invalid != 1 {
		t.Fatal("ledger/status drift", report.Totals)
	}
	event := func(window, name string) cpamp.Event {
		t.Helper()
		p, e := r.Evidence(ctx, Query{}, window)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range p.Events {
			if v.Hash == name {
				return v
			}
		}
		t.Fatal("missing event", window, name)
		return cpamp.Event{}
	}
	mixed := event("a-old", "known-mixed")
	if mixed.Value.Low != 7250000000 || mixed.Value.High != mixed.Value.Low || mixed.Value.Tokens.WriteAssumed5m != 0 {
		t.Fatal("known mixed split changed", mixed.Value)
	}
	old := event("a-old", "unknown-crossing")
	week := event("a-week", "unknown-crossing")
	if old.Value.Low != 5e9 || old.Value.High != 8e9 || old.Value.Tokens.WriteUnknown != 1e6 || week.Value.Low != 5e9 || week.Value.High != 5e9 || week.Value.Tokens.WriteAssumed5m != 1e6 {
		t.Fatal("whole window contextual valuation", old.Value, week.Value)
	}
	h := event("a-h", "enriched-mixed")
	if h.Value.Low != 6500000000 || h.Value.High != h.Value.Low || h.Value.Tokens.Write5m != 250000 || h.Value.Tokens.Write1h != 500000 || h.Value.Tokens.WriteAssumed5m != 250000 {
		t.Fatal("H endpoint / mixed enrichment", h.Value)
	}
	outside := event("a-h", "recorded-after-H")
	if outside.Value.Tokens.WriteAssumed5m != 0 || outside.Value.High != 8e9 {
		t.Fatal("outside H assumed", outside.Value)
	}
	early, e := r.Evidence(ctx, Query{}, "b-early")
	if e != nil {
		t.Fatal(e)
	}
	if early.Window.AttributionEnd != 1790726400000 || !early.Valuation.Assume5m || early.Window.Requests != 1 || early.Value.Tokens.WriteAssumed5m != 1000000 {
		t.Fatal("canonical early reset", early.Window, early.Valuation)
	}
	for _, id := range []string{"c-early", "d-h-band", "a-current"} {
		p, e := r.Evidence(ctx, Query{}, id)
		if e != nil {
			t.Fatal(e)
		}
		if p.Valuation.Assume5m || p.Value.Tokens.WriteAssumed5m != 0 {
			t.Fatal("ambiguous/current assumed", id, p.Valuation)
		}
	}
	proof, e := r.Evidence(ctx, Query{}, "a-h")
	if e != nil {
		t.Fatal(e)
	}
	if proof.Normalization.BoundaryRequests == 0 || proof.Normalization.USDPerPercent == nil || proof.Normalization.USDPerPercent.Low == proof.Normalization.USDPerPercent.High {
		t.Fatal("allocation bounds collapsed", proof.Normalization)
	}
	for _, p := range report.Profiles {
		if p.ID == "gap" && (len(p.Windows) != 0 || p.Selected.Requests != 2) {
			t.Fatal("fabricated meter or dropped gap history", p)
		}
	}
	for _, c := range cases(t) {
		if c.Name == "v2" {
			legacy, e := r.Evidence(ctx, Query{Pin: &c.Filter}, "b-early")
			if e != nil {
				t.Fatal(e)
			}
			if legacy.Valuation != nil || legacy.Window.Status != "superseded" || legacy.Value.Tokens.WriteAssumed5m != 0 {
				t.Fatal("v2 semantics changed")
			}
		}
	}
}

func TestNativeReferencesPagingAndRestart(t *testing.T) {
	r := fixtureReader(t)
	ctx := context.Background()
	ps, e := r.Profiles(ctx, -1)
	if e != nil || ps.Revision != 7 || len(ps.Profiles) != 5 {
		t.Fatal(ps, e)
	}
	ref := "profile:" + hash("main")
	q := WindowQuery{Query: Query{ProfileReference: ref}, State: "completed", Limit: 1}
	page, e := r.ListWindows(ctx, q)
	if e != nil || len(page.Windows) != 1 || page.NextCursor == "" {
		t.Fatal(page, e)
	}
	first := page.Windows[0].ID
	cursor := page.NextCursor
	path := r.ArtifactPath()
	r.Close()
	r, e = Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	q.Cursor = cursor
	seen := map[string]bool{first: true}
	for i := 0; i < 10; i++ {
		page, e = r.ListWindows(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		for _, w := range page.Windows {
			if seen[w.ID] {
				t.Fatal("duplicate page window")
			}
			seen[w.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatal("missing completed main windows", seen)
	}
	q.Cursor = cursor
	q.Kind = "weekly"
	if _, e = r.ListWindows(ctx, q); !errors.Is(e, ErrCursor) {
		t.Fatal("cross-filter cursor", e)
	}
	q.Kind = ""
	q.Cursor = cursor + "x"
	if _, e = r.ListWindows(ctx, q); !errors.Is(e, ErrCursor) {
		t.Fatal("tampered cursor", e)
	}
	other := fixtureReader(t)
	q.Cursor = cursor
	if _, e = other.ListWindows(ctx, q); !errors.Is(e, ErrCursor) {
		t.Fatal("cross-preparation cursor", e)
	}
	page, e = r.ListWindows(ctx, WindowQuery{State: "current", Limit: 100})
	if e != nil || len(page.Windows) == 0 {
		t.Fatal(page, e)
	}
	for _, w := range page.Windows {
		if w.Status != "current" {
			t.Fatal(w.Status)
		}
	}
	completed, e := r.ListWindows(ctx, WindowQuery{Limit: 100})
	if e != nil {
		t.Fatal(e)
	}
	for _, w := range completed.Windows {
		if w.ID == "d-h-band" || w.ID == "c-early" {
			t.Fatal("uncertain completion paged as completed", w.ID)
		}
	}
	if _, e = r.db.Exec(`DELETE FROM analytics_events`); e == nil {
		t.Fatal("prepared view writable")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e = os.Stat(path + suffix); !os.IsNotExist(e) {
			t.Fatal("read side effect")
		}
	}
	// Native references translate only filters. Original IDs and report pins stay exact.
	native, e := r.Report(ctx, Query{ProfileReference: ref})
	if e != nil || native.Filter.Profile != "main" {
		t.Fatal(native.Filter, e)
	}
	initial, e := r.Profiles(ctx, 0)
	if e != nil || len(initial.Profiles) != 0 {
		t.Fatal(initial, e)
	}
}

func TestBoundsCancellationAndSourceIsolation(t *testing.T) {
	s, b := frozenFixture(t)
	before, e := s.Status(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	r, e := Prepare(context.Background(), s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	after, _ := s.Status(context.Background())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("archive publication changed")
	}
	var settings int
	if r.db.QueryRow(`SELECT count(*) FROM settings`).Scan(&settings) != nil || settings != 0 {
		t.Fatal("private settings materialized")
	}
	for _, change := range []func(*Pin){func(p *Pin) { p.At = b.HMS + 1 }, func(p *Pin) { p.Through = b.EventsThrough + 1 }, func(p *Pin) { p.ObservationsThrough = b.ObservationsThrough + 1 }, func(p *Pin) { p.TokenEvidenceThrough++ }} {
		p := r.DefaultPin()
		change(&p)
		if _, e = r.Report(context.Background(), Query{Pin: &p}); !errors.Is(e, ErrBoundary) {
			t.Fatal(e)
		}
	}
	p := r.DefaultPin()
	if _, e = r.Report(context.Background(), Query{Pin: &p, ProfileReference: "unassigned"}); !errors.Is(e, ErrInput) {
		t.Fatal("pin overridden")
	}
	for _, q := range []Query{{ProfileReference: "main"}, {KeyHashes: []string{"sk-secret"}}, {RequestPage: -1}, {From: 2, To: 1}} {
		if _, e = r.Report(context.Background(), q); !errors.Is(e, ErrInput) {
			t.Fatal(e)
		}
	}
	if _, e = r.ListWindows(context.Background(), WindowQuery{Limit: 101}); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = r.Report(ctx, Query{}); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	if _, e = r.ListWindows(ctx, WindowQuery{}); !errors.Is(e, context.Canceled) {
		t.Fatal("page cancellation lost", e)
	}
	if _, e = r.Profiles(ctx, -1); !errors.Is(e, context.Canceled) {
		t.Fatal("profile cancellation lost", e)
	}
	if _, e = r.Evidence(ctx, Query{}, "a-week"); !errors.Is(e, context.Canceled) {
		t.Fatal("evidence cancellation lost", e)
	}
	var out bytes.Buffer
	if e = r.ExportJSON(context.Background(), Query{}, ExportOptions{}, &out); e != nil || strings.Contains(out.String(), "sk-synthetic-private-material") {
		t.Fatal("raw archive field leaked", e)
	}
	var wg sync.WaitGroup
	errch := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, e := r.Report(context.Background(), Query{})
			if e == nil && rep.Totals.Requests != 40 {
				e = ErrSource
			}
			errch <- e
		}()
	}
	wg.Wait()
	close(errch)
	for e := range errch {
		if e != nil {
			t.Fatal(e)
		}
	}
}

func TestIndependentPreparedArtifactAndCachedDTOOwnership(t *testing.T) {
	s, _ := frozenFixture(t)
	ctx := context.Background()
	r, e := Prepare(ctx, s, t.TempDir(), PrepareOptions{PollIntervalSeconds: 1200})
	if e != nil {
		t.Fatal(e)
	}
	path := r.ArtifactPath()
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	r.Close()
	r, e = Open(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if r.service.PollInterval.Seconds() != 1200 {
		t.Fatal("source freshness setting not frozen")
	}
	first, e := r.Report(ctx, Query{})
	if e != nil {
		t.Fatal(e)
	}
	first.Profiles[0].Name = "mutated by caller"
	first.PriceBook.Prices = nil
	first.Config.Profiles[0].Name = "mutated config"
	second, e := r.Report(ctx, Query{})
	if e != nil {
		t.Fatal(e)
	}
	if second.Profiles[0].Name == "mutated by caller" || second.Config.Profiles[0].Name == "mutated config" || len(second.PriceBook.Prices) == 0 {
		t.Fatal("cache exposed mutable shared storage")
	}
	if first.NextTransition != second.NextTransition {
		t.Fatal("cache lost domain transition deadline")
	}
	if _, e = Prepare(ctx, s, t.TempDir(), PrepareOptions{PollIntervalSeconds: 1}); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
}

func TestNewV3BookAndCohortAreComputedNatively(t *testing.T) {
	s, _ := frozenFixture(t, func(db *sql.DB) {
		if _, e := db.Exec(`DELETE FROM analytics_normalization_rules WHERE id LIKE 'profile-api-value-v3:%';DELETE FROM analytics_historical_valuations;DELETE FROM analytics_state WHERE name='historical_valuation'`); e != nil {
			t.Fatal(e)
		}
	})
	ctx := context.Background()
	before, e := s.Status(ctx)
	if e != nil {
		t.Fatal(e)
	}
	r, e := Prepare(ctx, s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	computed, e := r.Report(ctx, Query{})
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range cases(t) {
		if c.Name == "v3" {
			exactJSON(t, "native-created v3 policy/book", computed, c.Report)
		}
	}
	after, e := s.Status(ctx)
	if e != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("native preparation rewrote source provenance", e)
	}
	if computed.PriceBook.Captured != r.meta.Boundary.HMS || computed.PriceBook.Historical == nil || computed.PriceBook.Historical.SourceCheckpoint != r.meta.Boundary.CheckpointSHA256 {
		t.Fatal("default provenance not H/checkpoint anchored")
	}
}

func TestV2SchemaReplayAndNewPolicyWithoutInventedEnrichment(t *testing.T) {
	s, _ := frozenFixture(t, func(db *sql.DB) {
		if _, e := db.Exec(`DELETE FROM analytics_normalization_rules WHERE id LIKE 'profile-api-value-v3:%';DROP TABLE analytics_historical_valuations;DROP TABLE analytics_token_evidence;DELETE FROM analytics_state WHERE name='historical_valuation'`); e != nil {
			t.Fatal(e)
		}
	})
	ctx := context.Background()
	r, e := Prepare(ctx, s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	for _, c := range cases(t) {
		if c.Name == "v2" {
			replay, e := r.Report(ctx, Query{Pin: &c.Filter})
			if e != nil {
				t.Fatal(e)
			}
			exactJSON(t, "old schema v2 replay", replay, c.Report)
		}
	}
	if r.DefaultPin().TokenEvidenceThrough != 0 {
		t.Fatal("invented token revision")
	}
	p, e := r.Evidence(ctx, Query{}, "a-h")
	if e != nil {
		t.Fatal(e)
	}
	for _, event := range p.Events {
		if event.Hash == "enriched-mixed" && (event.TokenEvidence != nil || event.Value.Low != 5e9 || event.Value.High != 5e9 || event.Value.Tokens.WriteAssumed5m != 1e6) {
			t.Fatal("invented split or lost new-policy provenance", event)
		}
	}
}

func TestPrecutoffPreparationDoesNotInheritFutureCohort(t *testing.T) {
	s, _ := frozenFixture(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reports.sqlite")
	if _, e := s.MaterializeReports(ctx, path); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite3", uri(path, "mode=rw"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	at := cpamp.HistoricalValuationCutoff - 1
	service := cpamp.New(db, at)
	book, e := service.PrepareBook(ctx, cpamp.HistoricalImportCheckpoint{SourceCheckpoint: strings.Repeat("c", 64), At: at, EventsThrough: 40, ObservationsThrough: 18})
	if e != nil || book.Historical != nil || book.Captured != at {
		t.Fatal("pre-cutoff book inherited a future cohort", book, e)
	}
	for _, c := range cases(t) {
		if c.Name == "v3" {
			retained, e := service.Book(ctx, c.Filter.ValuationRule)
			if e != nil || retained.Historical == nil || retained.Historical.At <= at {
				t.Fatal("retained future book was rewritten", e)
			}
		}
	}
}
