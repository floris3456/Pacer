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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	// Checked before anything is created, so no oracle can land in a checkout.
	if e := realRefuseInsideWorktree(out); e != nil {
		t.Fatal(e)
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
		// The original code is built for this binary's GOARCH (float results differ
		// between arm64 and amd64); real-boundary.json records it for the gate.
		cmd.Env = append(os.Environ(),
			"GOARCH="+runtime.GOARCH,
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
	oracle, e := os.ReadFile(filepath.Join(out, "real-oracle.json"))
	if e != nil {
		t.Fatal(e)
	}
	// A generation that wrote zero cases is a failure, not an empty success.
	if e = realCheckWritten(oracle, meta.Cases); e != nil {
		t.Fatal(e)
	}
	t.Logf("real oracle: %d original cases written to scratch output (contents not logged)", meta.Cases)
}

// realRefuseInsideWorktree refuses an output directory inside the git worktree
// that runs the generator. Oracles hold original case data and must never be
// committed or left in a checkout by accident. It fails closed when git cannot
// report the worktree root.
func realRefuseInsideWorktree(out string) error {
	top, e := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if e != nil {
		return fmt.Errorf("cannot resolve git worktree root: %w", e)
	}
	root, e := realResolve(strings.TrimSpace(string(top)))
	if e != nil {
		return e
	}
	dir, e := realResolve(out)
	if e != nil {
		return e
	}
	if realWithin(dir, root) {
		return fmt.Errorf("output directory is inside the git worktree %s; choose a directory outside it", root)
	}
	return nil
}

// realResolve returns the absolute path of p with symlinks resolved. The output
// directory does not exist yet, so the longest existing ancestor is resolved and
// the missing tail is appended unchanged.
func realResolve(p string) (string, error) {
	abs, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	existing, tail := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			return filepath.Join(resolved, tail), nil
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing ancestor for %s", abs)
		}
		tail = filepath.Join(filepath.Base(existing), tail)
		existing = parent
	}
}

// realWithin reports whether dir is root or lies beneath it. Both arguments are
// cleaned by filepath.Rel, so ".." segments cannot escape the root.
func realWithin(dir, root string) bool {
	rel, e := filepath.Rel(root, dir)
	if e != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// realCheckWritten rejects a generation that wrote no cases, or whose oracle
// disagrees with the boundary's case total. Only counts appear in its errors.
func realCheckWritten(oracle []byte, boundaryCases int) error {
	var cases []json.RawMessage
	if e := json.Unmarshal(oracle, &cases); e != nil {
		return fmt.Errorf("real oracle is not a JSON array: %w", e)
	}
	if len(cases) == 0 {
		return fmt.Errorf("generation wrote 0 cases")
	}
	if len(cases) != boundaryCases {
		return fmt.Errorf("oracle has %d cases, boundary records %d", len(cases), boundaryCases)
	}
	return nil
}

// TestRealOutputOutsideWorktree: an oracle written inside a git worktree could be
// committed by accident, so generation must refuse such a directory.
func TestRealOutputOutsideWorktree(t *testing.T) {
	here, e := filepath.Abs(".")
	if e != nil {
		t.Fatal(e)
	}
	if err := realRefuseInsideWorktree(filepath.Join(here, "scratch-oracle")); err == nil {
		t.Fatal("output directory inside the git worktree was accepted")
	}
	if err := realRefuseInsideWorktree(filepath.Join(t.TempDir(), "oracle")); err != nil {
		t.Fatalf("output directory outside the worktree refused: %v", err)
	}
}

// TestRealWithin covers the path rule itself, independent of the git checkout.
func TestRealWithin(t *testing.T) {
	for _, tc := range []struct {
		dir, root string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/a/b/../x", "/a/b", false},
		{"/x/y", "/a/b", false},
	} {
		if got := realWithin(tc.dir, tc.root); got != tc.want {
			t.Errorf("realWithin(%q, %q) = %t, want %t", tc.dir, tc.root, got, tc.want)
		}
	}
}

// TestRealGenerationRefusesEmptyOracle: a child run that writes zero cases must
// fail generation rather than report success over an empty oracle.
func TestRealGenerationRefusesEmptyOracle(t *testing.T) {
	if err := realCheckWritten([]byte("null"), 0); err == nil {
		t.Fatal("zero written cases accepted")
	}
	if err := realCheckWritten([]byte("[]"), 0); err == nil {
		t.Fatal("empty oracle accepted")
	}
	if err := realCheckWritten([]byte(`[{"view":"profiles"}]`), 2); err == nil {
		t.Fatal("boundary total differing from oracle accepted")
	}
	if err := realCheckWritten([]byte(`[{"view":"profiles"}]`), 1); err != nil {
		t.Fatalf("valid oracle refused: %v", err)
	}
}
