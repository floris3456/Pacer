package pacer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func reportFixture(t *testing.T) *historyreports.Reader {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "frozen.sqlite")
	f, err := os.Open("../historyreports/testdata/golden/fixture.sql.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	sqlBytes, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(sqlBytes)); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(source, 0400); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	archive, err := history.Open(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	boundary := history.Boundary{Schema: "cpamp-usage-v3", SnapshotSHA256: hex.EncodeToString(digest[:]), LineageSHA256: strings.Repeat("a", 64), CheckpointSHA256: strings.Repeat("c", 64), HMS: 1790812800000, EventsThrough: 40, ObservationsThrough: 18}
	progress, err := archive.Import(context.Background(), history.ImportOptions{SourcePath: source, Boundary: boundary, BatchSize: 25})
	if err != nil || !progress.Complete {
		t.Fatal(progress, err)
	}
	reader, err := historyreports.Prepare(context.Background(), archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestNativeHistoricalManagementIsBoundedAndPreservesPinnedExports(t *testing.T) {
	p := startPlugin(t, filepath.Join(t.TempDir(), "state.sqlite"))
	defer p.Close()
	call := func(path string, body any) pluginapi.ManagementResponse {
		raw, _ := json.Marshal(body)
		return p.manage(pluginapi.ManagementRequest{Method: "POST", Path: path, Body: raw})
	}
	if got := call(historyRoot+"report", map[string]any{}); got.StatusCode != 409 {
		t.Fatal("missing configured artifact fabricated a report", got.StatusCode)
	}
	p.reports = reportFixture(t)
	profiles := p.manage(pluginapi.ManagementRequest{Method: "GET", Path: historyRoot + "profiles"})
	var list struct {
		Profiles []historyreports.Profile `json:"profiles"`
		Pin      historyreports.Pin       `json:"default_pin"`
	}
	if profiles.StatusCode != 200 || json.Unmarshal(profiles.Body, &list) != nil || len(list.Profiles) != 5 || list.Pin.Through != 40 {
		t.Fatal(string(profiles.Body))
	}
	if bytes.Contains(profiles.Body, []byte("artifact_path")) || bytes.Contains(profiles.Body, []byte(p.reports.ArtifactPath())) {
		t.Fatal("trusted artifact path exposed")
	}
	query := historyreports.Query{Pin: &list.Pin}
	report := call(historyRoot+"report", reportRequest{Query: query})
	if report.StatusCode != 200 {
		t.Fatal(string(report.Body))
	}
	var workspace historyreports.Workspace
	if json.Unmarshal(report.Body, &workspace) != nil || workspace.Filter.Through != 40 {
		t.Fatal("report advanced frozen pin")
	}
	windows := call(historyRoot+"windows", reportRequest{Query: query, Limit: 1, State: "completed"})
	if windows.StatusCode != 200 {
		t.Fatal(string(windows.Body))
	}
	var page historyreports.WindowPage
	if json.Unmarshal(windows.Body, &page) != nil || len(page.Windows) != 1 {
		t.Fatal(string(windows.Body))
	}
	if got := call(historyRoot+"windows", reportRequest{Query: query, Limit: 1, Cursor: "tampered"}); got.StatusCode != 409 {
		t.Fatal("tampered cursor accepted", string(got.Body))
	}
	for _, format := range []string{"json", "csv"} {
		var want bytes.Buffer
		var err error
		options := historyreports.ExportOptions{CSV: "daily"}
		if format == "json" {
			err = p.reports.ExportJSON(context.Background(), query, options, &want)
		} else {
			err = p.reports.ExportCSV(context.Background(), query, options, &want)
		}
		if err != nil {
			t.Fatal(err)
		}
		got := call(historyRoot+"export", reportRequest{Query: query, CSV: "daily", Format: format})
		var envelope struct {
			Content  string `json:"content"`
			Filename string `json:"filename"`
		}
		if got.StatusCode != 200 || json.Unmarshal(got.Body, &envelope) != nil || envelope.Content != want.String() || envelope.Filename != "pacer-history."+format {
			t.Fatal("HTTP download changed pinned writer bytes", format, string(got.Body))
		}
	}
	for _, request := range []pluginapi.ManagementRequest{
		{Method: "GET", Path: "/v0/resource/plugins/pacer/history/profiles"},
		{Method: "POST", Path: "/v0/resource/plugins/pacer/history/report", Body: []byte(`{}`)},
		{Method: "POST", Path: historyRoot + "prepare", Body: []byte(`{}`)},
	} {
		if got := p.manage(request); got.StatusCode != 404 {
			t.Fatal("public/preparation path exposed")
		}
	}
	for _, raw := range []string{`null`, `{"archive_path":"/private/path"}`, `{"query":{"model":"x"},"cursor":"ignored"}`, `{} {}`, `{"query":{"key_hashes":["not-a-digest"]}}`, `{"query":{},"query":{"model":"other"}}`, `{"Query":{}}`, `{"query":{"Model":"x"}}`, `{"query":{"model":"x","model":"y"}}`, `{"query":{"model":null}}`, `{"query":{"pin":null}}`, `{"query":null}`, `{"query":{},"limit":0}`} {
		got := p.manage(pluginapi.ManagementRequest{Method: "POST", Path: historyRoot + "report", Body: []byte(raw)})
		if got.StatusCode != 400 {
			t.Fatal("invalid request accepted", raw, string(got.Body))
		}
	}
	p.reportSlots <- struct{}{}
	p.reportSlots <- struct{}{}
	got := call(historyRoot+"report", reportRequest{})
	<-p.reportSlots
	<-p.reportSlots
	if got.StatusCode != 429 || got.Headers.Get("Retry-After") != "1" {
		t.Fatal("unbounded report concurrency")
	}
	var bounded boundedReport
	if _, err := bounded.Write(make([]byte, maxHistoryOutput+1)); err != errReportSize || bounded.Len() != 0 {
		t.Fatal("unbounded export body")
	}
}
