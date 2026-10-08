package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReportPreparationRejectsMissingOrForeignSourcesWithoutMutatingThem(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sqlite")
	for _, args := range [][]string{{}, {"--archive", missing, "--output-dir", dir}, {"--archive", missing, "--output-dir", dir, "--poll-interval-seconds", "1"}, {"--unknown", "private-fixture-value"}} {
		var out bytes.Buffer
		if err := prepareReports(args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid preparation succeeded or emitted an artifact")
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing archive was initialized")
	}
	foreign := filepath.Join(dir, "foreign.sqlite")
	content := []byte("not a Pacer archive")
	if err := os.WriteFile(foreign, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareReports([]string{"--archive", foreign, "--output-dir", dir}, new(bytes.Buffer)); err == nil {
		t.Fatal("foreign input accepted")
	}
	got, err := os.ReadFile(foreign)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatal("foreign input changed", err)
	}
}
