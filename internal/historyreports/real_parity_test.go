package historyreports

// Env-gated parity between the ORIGINAL CPAMP outputs written by
// TestGenerateRealOracle and Pacer's prepared artifact, opened read-only the same
// way internal/pacer opens report_path. Nothing is committed from the data.
// Failure output and parity-report.json carry only counts, views, case names and
// JSON pointers, never values.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp/exports"
)

// realParityFailure is one mismatching comparison. A case can produce up to three
// (domain JSON, export JSON, export CSV). FirstDiffPath is a JSON pointer into the
// domain value, or "/exports/<format>[...]" for export bodies.
type realParityFailure struct {
	View          string `json:"view"`
	Name          string `json:"name"`
	Window        string `json:"window"`
	Kind          string `json:"kind"`
	FirstDiffPath string `json:"first_diff_path"`
}

// realParityReport: Cases is every original case; Passed counts cases with no
// mismatch; Failed lists every mismatching comparison (never stops at the first).
// Caps and Dropped are copied from real-boundary.json. Dropped holds, per capped
// view, the ids found and the ids kept. Exhaustive is true only when nothing was
// dropped. It is informational: the test does not fail on it.
type realParityReport struct {
	Cases      int                     `json:"cases"`
	Passed     int                     `json:"passed"`
	Caps       map[string]int          `json:"caps"`
	Dropped    map[string]realCoverage `json:"dropped"`
	Exhaustive bool                    `json:"exhaustive"`
	Failed     []realParityFailure     `json:"failed"`
}

// realCoverage mirrors the per-view found/kept counts written by the service oracle.
type realCoverage struct {
	Found int `json:"found"`
	Kept  int `json:"kept"`
}

// realBoundary is the part of real-boundary.json the parity gate reads. GoArch
// is the GOARCH the original code ran under.
type realBoundary struct {
	Cases      int                     `json:"cases"`
	CaseCounts map[string]int          `json:"case_counts"`
	Caps       map[string]int          `json:"caps"`
	Coverage   map[string]realCoverage `json:"coverage"`
	GoArch     string                  `json:"goarch"`
}

// realParityGate lists every reason the run cannot count as a pass. A green run
// needs a non-empty oracle, every case passing, and a boundary file that
// describes exactly the cases replayed. It never inspects individual
// comparisons. Reasons are sorted so the output is deterministic.
func realParityGate(cases []historicalCase, report realParityReport, b realBoundary) []string {
	var reasons []string
	if len(cases) == 0 {
		reasons = append(reasons, "oracle has 0 cases")
	}
	if report.Passed != len(cases) {
		reasons = append(reasons, fmt.Sprintf("%d of %d cases passed", report.Passed, len(cases)))
	}
	if b.Cases != len(cases) {
		reasons = append(reasons, fmt.Sprintf("boundary records %d cases, oracle has %d", b.Cases, len(cases)))
	}
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.View]++
	}
	if !reflect.DeepEqual(counts, b.CaseCounts) {
		reasons = append(reasons, "per-view case counts differ from boundary")
	}
	// The compiler fuses x*y+z on arm64 but not on amd64, so float outputs are
	// only comparable when the oracle and this replay share one GOARCH.
	if b.GoArch != runtime.GOARCH {
		reasons = append(reasons, fmt.Sprintf("oracle ran under GOARCH %q, replay under %q", b.GoArch, runtime.GOARCH))
	}
	views := make([]string, 0, len(b.Caps))
	for view := range b.Caps {
		views = append(views, view)
	}
	sort.Strings(views)
	for _, view := range views {
		cov, ok := b.Coverage[view]
		if !ok {
			reasons = append(reasons, "boundary has no coverage for capped view "+view)
			continue
		}
		// The kept count must be exactly what the cap allows; anything else is
		// fabricated or stale coverage.
		if cov.Kept != min(cov.Found, b.Caps[view]) {
			reasons = append(reasons, "coverage for "+view+" is inconsistent with its cap")
		}
	}
	return reasons
}

// realExhaustive reports whether every id found in every view was kept. A capped
// view without coverage is not exhaustive: absence is never treated as evidence.
func realExhaustive(b realBoundary) bool {
	for view := range b.Caps {
		if _, ok := b.Coverage[view]; !ok {
			return false
		}
	}
	for _, cov := range b.Coverage {
		if cov.Found != cov.Kept {
			return false
		}
	}
	return true
}

// realForecastMaxPages bounds the cursor walk so a cursor that never ends fails
// the comparison instead of hanging the harness.
const realForecastMaxPages = 10000

var errRealForecastPages = errors.New("forecast pages did not end or did not match total")

// realForecastPages returns every forecast of one window by following next_cursor
// until it is empty. Reading only the first page would compare a truncated list.
// The final page must account for the reported total.
func realForecastPages(ctx context.Context, r *Reader, selection LegacyQuery, window string, limit int) ([]Forecast, error) {
	var all []Forecast
	cursor := ""
	for range realForecastMaxPages {
		p, e := r.Forecasts(ctx, ForecastQuery{Selection: selection, WindowID: window, Limit: limit, Cursor: cursor})
		if e != nil {
			return nil, e
		}
		all = append(all, p.Forecasts...)
		if p.NextCursor == "" {
			if len(all) != p.Total {
				return nil, errRealForecastPages
			}
			return all, nil
		}
		cursor = p.NextCursor
	}
	return nil, errRealForecastPages
}

func TestRealOracleParity(t *testing.T) {
	out, artifact := os.Getenv("PACER_REAL_ORACLE_OUT"), os.Getenv("PACER_REAL_ARTIFACT")
	if out == "" || artifact == "" {
		t.Skip("explicit oracle output and prepared artifact required")
	}
	ctx := context.Background()
	// Same read-only entry point as internal/pacer report_path: no import, no init.
	r, e := Open(ctx, artifact)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	raw, e := os.ReadFile(filepath.Join(out, "real-oracle.json"))
	if e != nil {
		t.Fatal(e)
	}
	var cases []historicalCase
	if e = json.Unmarshal(raw, &cases); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile(filepath.Join(out, "real-exports.json"))
	if e != nil {
		t.Fatal(e)
	}
	var exports map[string]string
	if e = json.Unmarshal(raw, &exports); e != nil {
		t.Fatal(e)
	}
	raw, e = os.ReadFile(filepath.Join(out, "real-boundary.json"))
	if e != nil {
		t.Fatal(e)
	}
	var boundary realBoundary
	if e = json.Unmarshal(raw, &boundary); e != nil {
		t.Fatal(e)
	}
	report := realParityReport{
		Cases:      len(cases),
		Caps:       boundary.Caps,
		Dropped:    boundary.Coverage,
		Exhaustive: realExhaustive(boundary),
		Failed:     []realParityFailure{},
	}
	for _, tc := range cases {
		fails := realCompareCase(ctx, r, tc, exports)
		if len(fails) == 0 {
			report.Passed++
		}
		report.Failed = append(report.Failed, fails...)
	}
	b, e := json.MarshalIndent(report, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	// Written before the gate so a failing run still leaves its diagnostics behind.
	if e = os.WriteFile(filepath.Join(out, "parity-report.json"), append(b, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	t.Logf("%d of %d cases passed; %d mismatching comparisons; exhaustive=%t", report.Passed, report.Cases, len(report.Failed), report.Exhaustive)
	if reasons := realParityGate(cases, report, boundary); len(reasons) > 0 {
		t.Fatalf("parity gate: %s", strings.Join(reasons, "; "))
	}
	if len(report.Failed) > 0 {
		t.Fatalf("%d mismatching comparisons; see parity-report.json", len(report.Failed))
	}
}

// realCompareCase replays one original case through Pacer and returns every mismatch.
// The call mirrors TestHistoricalOriginalDomainAndControllerParity.
func realCompareCase(ctx context.Context, r *Reader, tc historicalCase, exports map[string]string) []realParityFailure {
	fail := func(path string) realParityFailure {
		return realParityFailure{View: tc.View, Name: tc.Name, Window: tc.Window, Kind: tc.Kind, FirstDiffPath: path}
	}
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
		// Every page, not the first: a single page would hide forecasts past the cursor.
		value, err = realForecastPages(ctx, r, q, tc.Window, 100)
	default:
		return []realParityFailure{fail("/view")}
	}
	if err != nil {
		// Only the class of failure is recorded; the error text can contain values.
		return []realParityFailure{fail("/error")}
	}
	var fails []realParityFailure
	if path, differ := realDomainDiff(value, tc.Output); differ {
		fails = append(fails, fail(path))
	}
	if tc.View == "forecasts" {
		return fails
	}
	for _, format := range []string{"json", "csv"} {
		var b bytes.Buffer
		var e error
		switch {
		case tc.View == "quota" && format == "json":
			e = r.ExportQuotaJSON(ctx, QuotaQuery{Pin: tc.Quota}, &b)
		case tc.View == "quota":
			e = r.ExportQuotaCSV(ctx, QuotaQuery{Pin: tc.Quota}, &b)
		case format == "json":
			e = r.ExportLegacyJSON(ctx, q, opts, &b)
		default:
			e = r.ExportLegacyCSV(ctx, q, opts, &b)
		}
		want, ok := exports[tc.View+"/"+tc.Name+"/"+format]
		if !ok {
			fails = append(fails, fail("/exports/"+format+"/no-oracle"))
			continue
		}
		if e != nil {
			fails = append(fails, fail("/exports/"+format+"/error"))
			continue
		}
		if path, differ := realExportDiff(format, tc.View, b.Bytes(), []byte(want)); differ {
			fails = append(fails, fail(path))
		}
	}
	return fails
}

// realDomainDiff compares the domain value exactly as exactJSON does: marshal the
// Pacer value and compare with the compacted original bytes. A history report is
// compared up to the order the original leaves undefined (realHistoryJSON).
func realDomainDiff(got any, want json.RawMessage) (string, bool) {
	b, e := json.Marshal(got)
	if e != nil {
		return "/marshal", true
	}
	var compact bytes.Buffer
	if json.Compact(&compact, want) != nil {
		return "/oracle-invalid", true
	}
	w := compact.Bytes()
	if _, ok := got.(LegacyHistoryReport); ok {
		b, w = realHistoryOrder(b, w, realHistoryJSON)
	}
	if bytes.Equal(b, w) {
		return "", false
	}
	if p, ok := realJSONPointer(b, w); ok {
		return p, true
	}
	// Bytes differ but the documents are structurally equal (formatting only).
	return "", true
}

// realExportDiff compares export bodies bytewise, except for CSV views the
// historical test already compares as row multisets (the original Go map
// iteration order is not a semantic difference there), and history exports,
// compared up to the order the original leaves undefined.
func realExportDiff(format, view string, got, want []byte) (string, bool) {
	if view == "history" {
		canonical := realHistoryJSON
		if format == "csv" {
			canonical = realHistoryCSV
		}
		got, want = realHistoryOrder(got, want, canonical)
	}
	if format == "json" {
		if bytes.Equal(got, want) {
			return "", false
		}
		if p, ok := realJSONPointer(got, want); ok {
			return "/exports/json" + p, true
		}
		return "/exports/json", true
	}
	if view == "profiles" || view == "profile-proof" || view == "history-proof" {
		gotRows, e1 := realCSVRows(got)
		wantRows, e2 := realCSVRows(want)
		if e1 != nil || e2 != nil {
			return "/exports/csv", true
		}
		if reflect.DeepEqual(gotRows, wantRows) {
			return "", false
		}
		n := min(len(gotRows), len(wantRows))
		i := 0
		for i < n && gotRows[i] == wantRows[i] {
			i++
		}
		return "/exports/csv/" + strconv.Itoa(i), true
	}
	if bytes.Equal(got, want) {
		return "", false
	}
	return "/exports/csv", true
}

// realCSVRows is csvRowBytes without t.Fatal, so one bad export becomes a recorded mismatch.
func realCSVRows(b []byte) ([]string, error) {
	rows, e := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if e != nil {
		return nil, e
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var encoded bytes.Buffer
		w := csv.NewWriter(&encoded)
		if e = w.Write(row); e != nil {
			return nil, e
		}
		w.Flush()
		if e = w.Error(); e != nil {
			return nil, e
		}
		out = append(out, encoded.String())
	}
	if len(out) > 1 {
		sort.Strings(out[1:])
	}
	return out, nil
}

// realHistoryTie is what fixes a window's place in an original history report.
// Derive ranges over a map and sorts one account's windows by reset_ms with
// sort.Slice; Report appends current_pools in that order and stable-sorts
// windows by current-first, then reset_ms. Adjacent windows of one account that
// agree on reset_ms and on being current therefore have no defined order.
type realHistoryTie struct {
	Account string `json:"account"`
	Status  string `json:"status"`
	Reset   int64  `json:"reset_ms"`
}

func (a realHistoryTie) same(b realHistoryTie) bool {
	return a.Account == b.Account && a.Reset == b.Reset && (a.Status == "current") == (b.Status == "current")
}

// realSortTies sorts, by bytes, each run of adjacent items whose ties are the
// same. Nothing moves across a run boundary.
func realSortTies(items [][]byte, ties []realHistoryTie) {
	for i := 0; i < len(items); {
		j := i + 1
		for j < len(items) && ties[j].same(ties[i]) {
			j++
		}
		run := items[i:j]
		sort.Slice(run, func(x, y int) bool { return bytes.Compare(run[x], run[y]) < 0 })
		i = j
	}
}

// realHistoryOrder canonicalizes both sides, or neither when either side cannot
// be, so a malformed body still meets the exact comparison.
func realHistoryOrder(got, want []byte, canonical func([]byte) ([]byte, bool)) ([]byte, []byte) {
	g, okGot := canonical(got)
	w, okWant := canonical(want)
	if !okGot || !okWant {
		return got, want
	}
	return g, w
}

// realHistoryJSON returns a history report document with the tie runs of
// windows and current_pools sorted. Every other byte is kept: ok is false unless
// rebuilding doc without sorting reproduces it exactly.
func realHistoryJSON(doc []byte) ([]byte, bool) {
	d := json.NewDecoder(bytes.NewReader(doc))
	if t, e := d.Token(); e != nil || t != json.Delim('{') {
		return nil, false
	}
	array := func(items [][]byte) []byte {
		return append(append([]byte{'['}, bytes.Join(items, []byte{','})...), ']')
	}
	var same, sorted bytes.Buffer
	same.WriteByte('{')
	sorted.WriteByte('{')
	for first := true; d.More(); first = false {
		t, e := d.Token()
		key, isKey := t.(string)
		var value json.RawMessage
		if e != nil || !isKey || d.Decode(&value) != nil {
			return nil, false
		}
		name, _ := json.Marshal(key)
		if !first {
			name = append([]byte{','}, name...)
		}
		name = append(name, ':')
		same.Write(name)
		sorted.Write(name)
		if key != "windows" && key != "current_pools" {
			same.Write(value)
			sorted.Write(value)
			continue
		}
		var raw []json.RawMessage
		if json.Unmarshal(value, &raw) != nil {
			return nil, false
		}
		items := make([][]byte, len(raw))
		ties := make([]realHistoryTie, len(raw))
		for i, item := range raw {
			if json.Unmarshal(item, &ties[i]) != nil {
				return nil, false
			}
			items[i] = item
		}
		same.Write(array(items))
		realSortTies(items, ties)
		sorted.Write(array(items))
	}
	if t, e := d.Token(); e != nil || t != json.Delim('}') {
		return nil, false
	}
	rest := doc[d.InputOffset():]
	same.WriteByte('}')
	same.Write(rest)
	sorted.WriteByte('}')
	sorted.Write(rest)
	if !bytes.Equal(same.Bytes(), doc) {
		return nil, false
	}
	return sorted.Bytes(), true
}

// realHistoryCSV is realHistoryJSON for the history CSV export, whose rows list
// windows and end in the window's JSON. The header row stays first.
func realHistoryCSV(doc []byte) ([]byte, bool) {
	rows, e := csv.NewReader(bytes.NewReader(doc)).ReadAll()
	if e != nil || len(rows) == 0 {
		return nil, false
	}
	items := make([][]byte, len(rows))
	ties := make([]realHistoryTie, len(rows))
	for i, row := range rows {
		var b bytes.Buffer
		w := csv.NewWriter(&b)
		if w.Write(row) != nil {
			return nil, false
		}
		w.Flush()
		items[i] = b.Bytes()
		if i > 0 && json.Unmarshal([]byte(row[len(row)-1]), &ties[i]) != nil {
			return nil, false
		}
	}
	if !bytes.Equal(bytes.Join(items, nil), doc) {
		return nil, false
	}
	realSortTies(items[1:], ties[1:])
	return bytes.Join(items, nil), true
}

// realJSONPointer returns the first differing RFC 6901 pointer. Object keys are
// visited in sorted order, so the result is deterministic and not necessarily the
// first byte offset. ok is false when either side is not valid JSON.
func realJSONPointer(got, want []byte) (string, bool) {
	var a, b any
	if realDecode(got, &a) != nil || realDecode(want, &b) != nil {
		return "", false
	}
	return realWalk(a, b, "")
}

func realDecode(b []byte, v *any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

func realWalk(a, b any, ptr string) (string, bool) {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return ptr, true
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			child := ptr + "/" + realPointerEscape.Replace(k)
			xv, inX := x[k]
			yv, inY := y[k]
			if !inX || !inY {
				return child, true
			}
			if p, d := realWalk(xv, yv, child); d {
				return p, true
			}
		}
		return "", false
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return ptr, true
		}
		for i := range x {
			if p, d := realWalk(x[i], y[i], ptr+"/"+strconv.Itoa(i)); d {
				return p, true
			}
		}
		return "", false
	default:
		if reflect.DeepEqual(a, b) {
			return "", false
		}
		return ptr, true
	}
}

var realPointerEscape = strings.NewReplacer("~", "~0", "/", "~1")

// TestPrepareRealSmokeArtifact is smoke-only. It prepares a Pacer artifact from a
// SQL snapshot the same way historicalFixture does, minus the synthetic private
// sentinels, so the harness is proven on synthetic data. Real artifacts come from
// the product import path and are never produced by this test.
func TestPrepareRealSmokeArtifact(t *testing.T) {
	sqlPath, out := os.Getenv("PACER_REAL_SMOKE_SQL"), os.Getenv("PACER_REAL_SMOKE_ARTIFACT_OUT")
	if sqlPath == "" || out == "" {
		t.Skip("smoke SQL snapshot and fresh artifact dir required")
	}
	ctx := context.Background()
	if e := os.Mkdir(out, 0700); e != nil {
		t.Fatal(e)
	}
	schema, e := os.ReadFile(sqlPath)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sqlite")
	db, e := sql.Open("sqlite3", source)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(schema)); e != nil {
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
	// Same boundary literals as historicalFixture (which the synthetic pin matches).
	boundary := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hash(string(raw)), LineageSHA256: strings.Repeat("b", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790812800000, EventsThrough: 240, ObservationsThrough: 65}
	s, e := history.Open(filepath.Join(dir, "archive.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	progress, e := s.Import(ctx, history.ImportOptions{SourcePath: source, Boundary: boundary, BatchSize: 13})
	if e != nil || !progress.Complete {
		t.Fatal("smoke import incomplete", e)
	}
	r, e := Prepare(ctx, s, out)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	matches, _ := filepath.Glob(filepath.Join(out, "historyreports-*", "reports.sqlite"))
	if len(matches) != 1 {
		t.Fatal("expected exactly one prepared smoke artifact")
	}
	t.Log("smoke artifact prepared")
}

// TestRealParityGate proves the run-level gate cannot pass vacuously. Every row
// is a distinct way a green result could hide missing or capped evidence.
func TestRealParityGate(t *testing.T) {
	two := []historicalCase{{View: "profiles"}, {View: "history"}}
	// Boundaries are decoded from JSON, the way real-boundary.json is read.
	boundary := func(goarch string) realBoundary {
		arch := ""
		if goarch != "" {
			arch = `,"goarch":"` + goarch + `"`
		}
		var b realBoundary
		if e := json.Unmarshal([]byte(`{"cases":2,"case_counts":{"profiles":1,"history":1},"caps":{"profiles":20},"coverage":{"profiles":{"found":3,"kept":3}}`+arch+`}`), &b); e != nil {
			t.Fatal(e)
		}
		return b
	}
	other := "amd64"
	if runtime.GOARCH == other {
		other = "arm64"
	}
	clean := boundary(runtime.GOARCH)
	okReport := realParityReport{Cases: 2, Passed: 2, Failed: []realParityFailure{}}
	for _, tc := range []struct {
		name     string
		cases    []historicalCase
		report   realParityReport
		boundary realBoundary
		wantFail bool
	}{
		{"clean run passes", two, okReport, clean, false},
		{"vacuous empty oracle fails", nil, realParityReport{Failed: []realParityFailure{}}, realBoundary{}, true},
		{"passed short of cases fails", two, realParityReport{Cases: 2, Passed: 1}, clean, true},
		{"boundary case total mismatch fails", two, okReport, withCases(clean, 3), true},
		{"per-view case count mismatch fails", two, okReport, withCounts(clean, map[string]int{"profiles": 2}), true},
		{"capped view without coverage fails", two, okReport, withCaps(clean, map[string]int{"profiles": 20, "accounts": 20}), true},
		// arm64 contracts x*y+z into fused multiply-add and amd64 (v1) does not, so
		// the same original code yields different last bits; such an oracle is not
		// comparable with this replay.
		{"oracle under another GOARCH fails", two, okReport, boundary(other), true},
		{"oracle without recorded GOARCH fails", two, okReport, boundary(""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reasons := realParityGate(tc.cases, tc.report, tc.boundary)
			if got := len(reasons) > 0; got != tc.wantFail {
				t.Fatalf("gate failed=%t (reasons=%v), want %t", got, reasons, tc.wantFail)
			}
		})
	}
}

func withCases(b realBoundary, n int) realBoundary { b.Cases = n; return b }

func withCounts(b realBoundary, c map[string]int) realBoundary { b.CaseCounts = c; return b }

func withCaps(b realBoundary, c map[string]int) realBoundary { b.Caps = c; return b }

// TestRealExhaustive: a view with dropped ids makes the run non-exhaustive, and
// a missing coverage entry must not be read as exhaustive.
func TestRealExhaustive(t *testing.T) {
	full := realBoundary{Caps: map[string]int{"forecasts": 50}, Coverage: map[string]realCoverage{"forecasts": {Found: 4, Kept: 4}}}
	if !realExhaustive(full) {
		t.Fatal("all ids kept but reported non-exhaustive")
	}
	dropped := full
	dropped.Coverage = map[string]realCoverage{"forecasts": {Found: 60, Kept: 50}}
	if realExhaustive(dropped) {
		t.Fatal("dropped ids reported exhaustive")
	}
	missing := full
	missing.Coverage = map[string]realCoverage{}
	if realExhaustive(missing) {
		t.Fatal("capped view without coverage reported exhaustive")
	}
}

// TestRealForecastPagesAgree checks the forecast parity reader against the
// single-page reader: a window read in one page of 100 and in pages of 1 must
// yield the same forecasts, and every page must be followed to its end.
func TestRealForecastPagesAgree(t *testing.T) {
	ctx := context.Background()
	r := historicalFixture(t)
	defer r.Close()
	// "forecast-window" holds two forecasts (see TestHistoricalForecastBoundsAndScenarioSemantics).
	const window = "forecast-window"
	one, e := realForecastPages(ctx, r, LegacyQuery{}, window, 100)
	if e != nil {
		t.Fatal(e)
	}
	paged, e := realForecastPages(ctx, r, LegacyQuery{}, window, 1)
	if e != nil {
		t.Fatal(e)
	}
	if len(one) != 2 || len(one) != len(paged) {
		t.Fatalf("single page %d forecasts, paged %d, want 2 and 2", len(one), len(paged))
	}
	for i := range one {
		if one[i].Forecast.ID != paged[i].Forecast.ID {
			t.Fatalf("forecast %d differs between page sizes", i)
		}
	}
}

// TestRealHistoryTieOrder: CPAMP leaves the order of one account's history
// windows with equal reset_ms undefined. Derive ranges over a Go map and then
// sorts by reset_ms alone with sort.Slice, and Report keeps that relative order
// (SliceStable on current-first, reset_ms) in windows and in current_pools. Two
// original runs on one scratch copy list such windows differently. The
// comparator must accept exactly that permutation in the domain value and both
// exports, and still reject every wider one.
func TestRealHistoryTieOrder(t *testing.T) {
	w := func(id, account, status string, reset, end int64) cpamp.Window {
		return cpamp.Window{ID: id, Account: account, Status: status, Kind: "5h", Scope: "scope-" + id, Reset: reset, AttributionEnd: end, ObservationIDs: []string{"obs-" + id}, Notes: []string{}}
	}
	a1 := w("a1", "acct-a", "current", 2000, 2000)
	a2 := w("a2", "acct-a", "current", 2000, 1500)
	b1 := w("b1", "acct-b", "current", 2000, 2000)
	b2 := w("b2", "acct-b", "superseded", 2000, 1800)
	a3 := w("a3", "acct-a", "closed", 1000, 1000)
	a4 := w("a4", "acct-a", "superseded", 1000, 900)
	report := func(pools, windows []cpamp.Window) LegacyHistoryReport {
		return LegacyHistoryReport{Version: "synthetic", APIKeys: []cpamp.APIKey{}, Accounts: []cpamp.Account{}, ManualQuotaProviders: []string{"meta"}, CurrentPools: pools, Windows: windows}
	}
	// Original order: accounts a then b; current first; reset_ms descending.
	oracle := report([]cpamp.Window{a1, a2, b1}, []cpamp.Window{a1, a2, b1, b2, a3, a4})
	changed := a2
	changed.AttributionEnd = 1600
	// csvEqual differs from wantEqual only where current_pools alone changed:
	// the CSV export lists windows and nothing else.
	for _, tc := range []struct {
		name      string
		got       LegacyHistoryReport
		wantEqual bool
		csvEqual  bool
	}{
		{"same order", oracle, true, true},
		{"same-account equal-reset windows swapped", report([]cpamp.Window{a2, a1, b1}, []cpamp.Window{a2, a1, b1, b2, a4, a3}), true, true},
		{"accounts interleaved", report([]cpamp.Window{a1, a2, b1}, []cpamp.Window{a1, b1, a2, b2, a3, a4}), false, false},
		{"current pool moved to another account", report([]cpamp.Window{a1, b1, a2}, []cpamp.Window{a1, a2, b1, b2, a3, a4}), false, true},
		{"current and non-current of one reset swapped", report([]cpamp.Window{a1, a2, b1}, []cpamp.Window{a1, a2, b2, b1, a3, a4}), false, false},
		{"reset order broken", report([]cpamp.Window{a1, a2, b1}, []cpamp.Window{a1, a2, b1, a3, b2, a4}), false, false},
		{"value changed inside a tie", report([]cpamp.Window{changed, a1, b1}, []cpamp.Window{changed, a1, b1, b2, a4, a3}), false, false},
		{"tie member duplicated", report([]cpamp.Window{a1, a1, b1}, []cpamp.Window{a1, a1, b1, b2, a3, a4}), false, false},
		{"tie member dropped", report([]cpamp.Window{a1, a2, b1}, []cpamp.Window{a1, a2, b1, b2, a3}), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, _ := json.Marshal(oracle)
			if _, differ := realDomainDiff(tc.got, want); differ == tc.wantEqual {
				t.Errorf("domain: differ=%t, want %t", differ, !tc.wantEqual)
			}
			var gotJSON, wantJSON, gotCSV, wantCSV bytes.Buffer
			_ = json.NewEncoder(&gotJSON).Encode(tc.got)
			_ = json.NewEncoder(&wantJSON).Encode(oracle)
			for _, x := range []struct {
				v   LegacyHistoryReport
				out *bytes.Buffer
			}{{tc.got, &gotCSV}, {oracle, &wantCSV}} {
				c := csv.NewWriter(x.out)
				exports.WriteHistoryReport(c, x.v)
				c.Flush()
			}
			if _, differ := realExportDiff("json", "history", gotJSON.Bytes(), wantJSON.Bytes()); differ == tc.wantEqual {
				t.Errorf("json export: differ=%t, want %t", differ, !tc.wantEqual)
			}
			if _, differ := realExportDiff("csv", "history", gotCSV.Bytes(), wantCSV.Bytes()); differ == tc.csvEqual {
				t.Errorf("csv export: differ=%t, want %t", differ, !tc.csvEqual)
			}
			// Views whose original order is fully defined stay strictly ordered.
			if !tc.wantEqual || tc.name == "same order" {
				return
			}
			for _, view := range []string{"profiles", "history-proof", "quota"} {
				if _, differ := realExportDiff("json", view, gotJSON.Bytes(), wantJSON.Bytes()); !differ {
					t.Errorf("%s json export accepted a reordering", view)
				}
			}
			if _, differ := realExportDiff("csv", "quota", gotCSV.Bytes(), wantCSV.Bytes()); !differ {
				t.Error("quota csv export accepted a reordering")
			}
		})
	}
}
