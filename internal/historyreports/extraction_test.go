package historyreports

// Opt-in provenance/build test for the additional historical read closure.
// The existing generator scripts remain unchanged. This test supplies additional
// roots to the same AST extractor in task-local test scratch, not a live checkout.
import (
	"bytes"
	"encoding/json"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractHistoricalReadClosure(t *testing.T) {
	source := os.Getenv("PACER_CPAMP_SOURCE")
	if source == "" {
		t.Skip("explicit frozen source checkout required for extraction")
	}
	sha, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(sha)) != "e8492c245bf69144afdcf20b8b4ea8935f0476e5" {
		t.Fatal("unexpected original source revision", err)
	}
	if err = exec.Command("git", "-C", source, "diff", "--quiet", "HEAD").Run(); err != nil {
		t.Fatal("modified original source checkout", err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := os.ReadFile("generate/extract.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(driver)
	text = strings.Replace(text, `"buildProfileWorkspace",`, `"ProfileReport", "ProfileProof", "Report", "ProofAt", "QuotaUsage", "forecastEvidence", "buildProfileWorkspace",`, 1)
	text = strings.Replace(text, `"ModelPrice", "ModelPriceForContext"`, `"ModelPriceForServiceTier", "ModelPrice", "ModelPriceForContext"`, 1)
	// Header stays the source extractor's own generated marker. Pin adaptations
	// are recorded separately here and exercised against the original oracle.
	tmp := t.TempDir()
	file := filepath.Join(tmp, "extract.go")
	if err = os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", file, source)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("extract: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
	path := filepath.Join(root, "internal/historyreports/internal/cpamp/service.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The original raw history report only has a live clock/max-cursor API.
	// Replace its inputs, not its derivation/calibration or report schema.
	old := `err = s.db.QueryRowContext(ctx, "SELECT coalesce((SELECT value FROM analytics_state WHERE name='events'),0),coalesce((SELECT max(rowid) FROM analytics_observations),0)").Scan(&r.Through, &r.ObservationThrough)`
	if bytes.Count(b, []byte(old)) != 1 {
		t.Fatal("raw history watermark source changed")
	}
	b = bytes.Replace(b, []byte(old), []byte(`r.Through, r.ObservationThrough = s.bounds.Through, s.bounds.ObservationsThrough`), 1)
	if err = os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	// Allow unpriced model normalization without importing collection/routing.
	manifest := map[string]any{
		"source_commit":    "e8492c245bf69144afdcf20b8b4ea8935f0476e5",
		"additional_roots": []string{"ProfileReport", "ProfileProof", "Report", "ProofAt", "QuotaUsage", "forecastEvidence"},
		"adaptations":      []string{"time.Now().UnixMilli() -> explicit service at", "raw Report SQL maxima -> explicit service bounds", "normalization initialized offline before readonly open", "controller CSV map-backed groups use deterministic order; row bytes unchanged"},
	}
	b, _ = json.MarshalIndent(manifest, "", "  ")
	if err = os.WriteFile(filepath.Join(root, "internal/historyreports/internal/cpamp/HISTORICAL-EXTRACTION.json"), append(b, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	extractHistoricalExports(t, root, source)
}

func extractHistoricalExports(t *testing.T, root, source string) {
	t.Helper()
	out := `// Code extracted from CPAMP; Copyright (c) 2026 Seakee, MIT (see LICENSE).
// Controller CSV bodies; map-backed row groups have explicit deterministic order.
package exports
import("encoding/csv";"encoding/json";"fmt";"strconv";"sort"; history "github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports/internal/cpamp")
`
	manifestPath := filepath.Join(root, "internal/historyreports/internal/cpamp/PROVENANCE.json")
	b, e := os.ReadFile(manifestPath)
	if e != nil {
		t.Fatal(e)
	}
	manifest := map[string]string{}
	if json.Unmarshal(b, &manifest) != nil {
		t.Fatal("manifest")
	}
	for _, item := range []struct{ file, header, name, params string }{
		{"profiles.go", `"row_type", "key"`, "WriteLegacyProfileReport", "report history.ProfileReport"},
		{"profiles.go", `"row_type", "id", "account"`, "WriteLegacyProfileProof", "p history.ProfileProof"},
		{"handler.go", `"window_id", "account"`, "WriteHistoryReport", "report history.Report"},
		{"handler.go", `"row_type", "id", "timestamp_ms"`, "WriteHistoryProof", "proof history.Proof"},
		{"quota_usage.go", `"window_id", "credential"`, "WriteQuotaUsage", "report history.QuotaUsageReport, f history.QuotaUsageFilter"},
	} {
		rel := "apps/manager-server/internal/http/controller/usagehistory/" + item.file
		b, e := os.ReadFile(filepath.Join(source, rel))
		if e != nil {
			t.Fatal(e)
		}
		manifest[rel] = hash(string(b))
		s := string(b)
		start := strings.Index(s, `_ = c.Write([]string{`+item.header)
		if start < 0 {
			t.Fatal("CSV header changed", item.name)
		}
		indent := s[strings.LastIndex(s[:start], "\n")+1 : start]
		end := strings.Index(s[start:], "\n"+indent+"return\n")
		if end < 0 {
			t.Fatal("CSV boundary changed", item.name)
		}
		body := s[start : start+end]
		body = strings.ReplaceAll(body, `for kind, rows := range map[string][]history.ProfileBucket{"day": report.Timeline, "model": report.Models, "client": report.Clients, "profile": report.ByProfile} {`, `for _, kind := range []string{"day", "model", "client", "profile"} { rows := map[string][]history.ProfileBucket{"day": report.Timeline, "model": report.Models, "client": report.Clients, "profile": report.ByProfile}[kind]`)
		body = strings.ReplaceAll(body, `for kind, value := range map[string]any{"window": p.Window, "filter": p.Filter, "normalization": p.Normalization} {`, `for _, kind := range []string{"window", "filter", "normalization"} { value := map[string]any{"window": p.Window, "filter": p.Filter, "normalization": p.Normalization}[kind]`)
		body = strings.ReplaceAll(body, `for id, price := range p.Prices {`, `ids := make([]string,0,len(p.Prices)); for id := range p.Prices { ids=append(ids,id) }; sort.Strings(ids); for _, id := range ids { price := p.Prices[id]`)
		body = strings.ReplaceAll(body, `for id, p := range proof.Prices {`, `ids := make([]string,0,len(proof.Prices)); for id := range proof.Prices { ids=append(ids,id) }; sort.Strings(ids); for _, id := range ids { p := proof.Prices[id]`)
		out += "\nfunc " + item.name + "(c *csv.Writer, " + item.params + "){ defer c.Flush();\n" + body + "\n}\n"
	}
	b, e = format.Source([]byte(out))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "internal/historyreports/internal/cpamp/exports/historical.go"), b, 0644); e != nil {
		t.Fatal(e)
	}
	b, _ = json.MarshalIndent(manifest, "", "  ")
	if e = os.WriteFile(manifestPath, append(b, '\n'), 0644); e != nil {
		t.Fatal(e)
	}
}
