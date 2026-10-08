package main

import (
	"path/filepath"
	"testing"
)

func TestInitializerIsExplicitAndRefusesAnExistingPolicyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	for _, args := range [][]string{nil, {"init", "--state", path}, {"init", "--state", path, "--routes", ""}, {"reset", "--state", path, "--routes", "claude-main"}} {
		if err := run(args); err == nil {
			t.Fatal("implicit initialization accepted", args)
		}
	}
	args := []string{"init", "--state", path, "--routes", "claude-main"}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	if err := run(args); err == nil {
		t.Fatal("existing policy schema silently recreated")
	}
}
