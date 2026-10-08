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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
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
type realParityReport struct {
	Cases  int                 `json:"cases"`
	Passed int                 `json:"passed"`
	Failed []realParityFailure `json:"failed"`
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
	report := realParityReport{Cases: len(cases), Failed: []realParityFailure{}}
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
	if e = os.WriteFile(filepath.Join(out, "parity-report.json"), append(b, '\n'), 0600); e != nil {
		t.Fatal(e)
	}
	t.Logf("%d of %d cases passed; %d mismatching comparisons", report.Passed, report.Cases, len(report.Failed))
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
		var p ForecastPage
		p, err = r.Forecasts(ctx, ForecastQuery{Selection: q, WindowID: tc.Window, Limit: 100})
		value = p.Forecasts
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
// Pacer value and compare with the compacted original bytes.
func realDomainDiff(got any, want json.RawMessage) (string, bool) {
	b, e := json.Marshal(got)
	if e != nil {
		return "/marshal", true
	}
	var compact bytes.Buffer
	if json.Compact(&compact, want) != nil {
		return "/oracle-invalid", true
	}
	if bytes.Equal(b, compact.Bytes()) {
		return "", false
	}
	if p, ok := realJSONPointer(b, compact.Bytes()); ok {
		return p, true
	}
	// Bytes differ but the documents are structurally equal (formatting only).
	return "", true
}

// realExportDiff compares export bodies bytewise, except for CSV views the
// historical test already compares as row multisets (the original Go map
// iteration order is not a semantic difference there).
func realExportDiff(format, view string, got, want []byte) (string, bool) {
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
