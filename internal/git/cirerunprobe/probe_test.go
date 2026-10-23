// Package cirerunprobe holds a deliberate flake that checks the CI re-run report: its tests fail on
// their first attempt of a CI run and pass on the re-run. It exists on a ci/** branch only and is
// removed after the check.
package cirerunprobe

import (
	"os"
	"path/filepath"
	"testing"
)

// failsOnFirstAttempt reports whether this is the first attempt of name in this run: the first one
// leaves a marker in the temporary directory, keyed by the GitHub run.
func failsOnFirstAttempt(t *testing.T, name string) bool {
	marker := filepath.Join(os.TempDir(), "gitai-rerun-probe-"+os.Getenv("GITHUB_RUN_ID")+"-"+os.Getenv("GITHUB_RUN_ATTEMPT")+"-"+name)
	if _, err := os.Stat(marker); err == nil {
		return false
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return true
}

func TestRerunProbe(t *testing.T) {
	if failsOnFirstAttempt(t, "test") {
		t.Fatalf("deliberate flake for the re-run report: expected 1, got 2")
	}
}

func TestRerunProbeParent(t *testing.T) {
	t.Run("child", func(t *testing.T) {
		if failsOnFirstAttempt(t, "child") {
			t.Errorf("deliberate flaky subtest for the re-run report")
		}
	})
}
