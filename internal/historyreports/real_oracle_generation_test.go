package historyreports

// Env-gated generation of the real-data oracle from the ORIGINAL CPAMP code.
//
// Nothing is committed from its output. The original module is never edited:
// two test files are overlaid (go test -overlay) and one line of the original
// report clock is replaced with the pin's explicit `at`, exactly as
// TestGenerateHistoricalOracle does. Every original calculation and handler runs
// unchanged against the caller's writable scratch copy of the backup.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const realCPAMPSourceSHA = "e8492c245bf69144afdcf20b8b4ea8935f0476e5"

// realChildTests are run in order: the service oracle writes real-oracle.json
// and real-boundary.json, then the controller export oracle reads them.
var realChildTests = []struct{ pkg, file, test string }{
	{"service", "real_service", "TestPacerRealServiceOracle"},
	{"http/controller", "real_exports", "TestPacerRealExportOracle"},
}

func TestGenerateRealOracle(t *testing.T) {
	source := os.Getenv("PACER_CPAMP_SOURCE")
	db := os.Getenv("PACER_REAL_DB")
	pin := os.Getenv("PACER_REAL_PIN")
	out := os.Getenv("PACER_REAL_ORACLE_OUT")
	if source == "" || db == "" || pin == "" || out == "" {
		t.Skip("explicit frozen source, scratch DB, fixed pin and fresh output dir required")
	}
	// Mkdir fails on an existing directory, so a previous oracle is never overwritten.
	if e := os.Mkdir(out, 0700); e != nil {
		t.Fatal(e)
	}
	if info, e := os.Stat(db); e != nil || !info.Mode().IsRegular() {
		// The original store opens with create semantics; a missing file would
		// silently produce an empty oracle instead of failing.
		t.Fatal("PACER_REAL_DB must be an existing regular file")
	}
	here, e := filepath.Abs(".")
	if e != nil {
		t.Fatal(e)
	}
	pinBytes, e := os.ReadFile(pin)
	if e != nil {
		t.Fatal(e)
	}
	var fixed struct {
		At int64 `json:"at"`
	}
	if e = json.Unmarshal(pinBytes, &fixed); e != nil || fixed.At <= 0 {
		t.Fatal("pin must carry a positive at")
	}
	sha, e := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if e != nil || strings.TrimSpace(string(sha)) != realCPAMPSourceSHA {
		t.Fatal("unexpected original source revision", e)
	}
	module := filepath.Join(source, "apps/manager-server")

	replace := map[string]string{}
	for _, item := range realChildTests {
		replace[filepath.Join(module, "internal", item.pkg, "usagehistory", "pacer_"+item.file+"_test.go")] = filepath.Join(here, "testdata", item.file+"_test.go.txt")
	}
	// The original Report has no clock parameter. Freeze only Report.Generated to
	// the pin's at; every other original calculation runs unchanged.
	servicePath := filepath.Join(module, "internal/service/usagehistory/service.go")
	b, e := os.ReadFile(servicePath)
	if e != nil {
		t.Fatal(e)
	}
	old := `Generated: time.Now().UnixMilli()`
	if strings.Count(string(b), old) != 1 {
		t.Fatal("original report clock changed")
	}
	frozen := strings.Replace(string(b), old, `Generated: int64(`+strconv.FormatInt(fixed.At, 10)+`)`, 1)
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	for _, item := range realChildTests {
		cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-timeout=55m", "-overlay", overlayPath, "-run", "^"+item.test+"$", "-v", "./internal/"+item.pkg+"/usagehistory")
		cmd.Dir = module
		cmd.Env = append(os.Environ(),
			"PACER_REAL_ORACLE_OUT="+out,
			"PACER_REAL_DB="+db,
			"PACER_REAL_PIN="+pin,
			"PACER_REAL_SOURCE_SHA="+realCPAMPSourceSHA,
		)
		// Child output can echo original error text; keep it in a scratch log and
		// report only the exit status here, never row contents.
		logPath := filepath.Join(out, item.test+".log")
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		runErr := cmd.Run()
		if e := os.WriteFile(logPath, log.Bytes(), 0600); e != nil {
			t.Fatal(e)
		}
		if runErr != nil {
			t.Fatalf("%s failed (%v); see %s", item.test, runErr, logPath)
		}
	}
	raw, e := os.ReadFile(filepath.Join(out, "real-boundary.json"))
	if e != nil {
		t.Fatal(e)
	}
	var meta struct {
		Cases int `json:"cases"`
	}
	if e = json.Unmarshal(raw, &meta); e != nil {
		t.Fatal(e)
	}
	t.Logf("real oracle: %d original cases written to scratch output (contents not logged)", meta.Cases)
}
