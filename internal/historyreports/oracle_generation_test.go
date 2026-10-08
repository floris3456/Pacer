package historyreports

// Explicit original-source oracle generation. No scripts/configuration are
// changed; Go overlays exist only in task-local test scratch.
import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateHistoricalOracle(t *testing.T) {
	source, out := os.Getenv("PACER_CPAMP_SOURCE"), os.Getenv("PACER_HISTORICAL_ORACLE_OUT")
	if source == "" || out == "" {
		t.Skip("explicit frozen source and fresh task-local output required")
	}
	if e := os.Mkdir(out, 0700); e != nil {
		t.Fatal(e)
	}
	here, e := filepath.Abs(".")
	if e != nil {
		t.Fatal(e)
	}
	module := filepath.Join(source, "apps/manager-server")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	sha, e := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(sha)) != "e8492c245bf69144afdcf20b8b4ea8935f0476e5" {
		t.Fatal("unexpected original source revision", e)
	}
	replace := map[string]string{}
	for _, item := range []struct{ pkg, file string }{{"service", "oracle_service"}, {"service", "historical_service"}, {"http/controller", "historical_exports"}} {
		replace[filepath.Join(module, "internal", item.pkg, "usagehistory", "pacer_"+item.file+"_test.go")] = filepath.Join(here, "testdata", item.file+"_test.go.txt")
	}
	// Original Report has no clock parameter. Freeze that one input only; run
	// all original calculations/controller bodies unchanged at the archive H.
	servicePath := filepath.Join(module, "internal/service/usagehistory/service.go")
	b, e := os.ReadFile(servicePath)
	if e != nil {
		t.Fatal(e)
	}
	old := `Generated: time.Now().UnixMilli()`
	if strings.Count(string(b), old) != 1 {
		t.Fatal("original report clock changed")
	}
	frozen := strings.Replace(string(b), old, `Generated: int64(1790812800000)`, 1)
	clockFile := filepath.Join(out, "original-service-clock.go.txt")
	if e = os.WriteFile(clockFile, []byte(frozen), 0600); e != nil {
		t.Fatal(e)
	}
	replace[servicePath] = clockFile
	overlay, _ := json.Marshal(map[string]any{"Replace": replace})
	overlayPath := filepath.Join(out, "overlay.json")
	if e = os.WriteFile(overlayPath, overlay, 0600); e != nil {
		t.Fatal(e)
	}
	for _, item := range []struct{ pkg, test string }{{"service", "TestPacerReportOracle"}, {"service", "TestPacerHistoricalOracle"}, {"http/controller", "TestPacerHistoricalExportOracle"}} {
		cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-timeout=180s", "-overlay", overlayPath, "-run", "^"+item.test+"$", "-v", "./internal/"+item.pkg+"/usagehistory")
		cmd.Dir = module
		cmd.Env = append(os.Environ(), "PACER_ORACLE_OUT="+out, "PACER_SCHEMA="+filepath.Join(here, "../history/cpamp-schema.sql"))
		b, e := cmd.CombinedOutput()
		os.WriteFile(filepath.Join(out, item.test+".log"), b, 0600)
		if e != nil {
			t.Fatalf("%s: %v\n%s", item.test, e, b)
		}
		t.Log(string(b))
	}
	target := filepath.Join(here, "testdata/historical-golden")
	if e = os.MkdirAll(target, 0755); e != nil {
		t.Fatal(e)
	}
	hashes := map[string]string{}
	for _, name := range []string{"historical-fixture.sql", "historical-oracle.json", "historical-exports.json", "historical-boundary.json"} {
		b, e := os.ReadFile(filepath.Join(out, name))
		if e != nil {
			t.Fatal(e)
		}
		hashes[name] = hash(string(b))
		f, e := os.Create(filepath.Join(target, name+".gz"))
		if e != nil {
			t.Fatal(e)
		}
		z, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
		if _, e = z.Write(b); e != nil {
			t.Fatal(e)
		}
		if e = z.Close(); e != nil {
			t.Fatal(e)
		}
		if e = f.Close(); e != nil {
			t.Fatal(e)
		}
	}
	b, _ = json.MarshalIndent(map[string]any{"source_commit": strings.TrimSpace(string(sha)), "clock_adaptation": "raw Report.Generated fixed to explicit H; original calculations/handlers unchanged", "outputs": hashes}, "", "  ")
	if e = os.WriteFile(filepath.Join(target, "provenance.json"), append(b, '\n'), 0644); e != nil {
		t.Fatal(e)
	}
}
