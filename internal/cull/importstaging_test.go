package cull

import (
	"os"
	"path/filepath"
	"testing"
)

// The sweep must clear the unreachable per-run directories older versions left
// behind WITHOUT touching the fixed one — that one holds the staged copies a
// failed import resumes from, so deleting it would restore the very bug this
// replaces (every RAF pulled off the camera again).
func TestSweepStaleStagingKeepsTheFixedDir(t *testing.T) {
	cache := t.TempDir()
	fixed := filepath.Join(cache, "import-stage")
	keep := filepath.Join(fixed, "DSCF0001.RAF")
	stale := []string{
		filepath.Join(cache, "import-stage-123456"),
		filepath.Join(cache, "import-stage-abcdef"),
	}
	unrelated := filepath.Join(cache, "thumbs")

	for _, d := range append([]string{fixed, unrelated}, stale...) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(keep, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range stale {
		if err := os.WriteFile(filepath.Join(d, "orphan.RAF"), []byte("leaked"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sweepStaleStaging(cache)

	if _, err := os.Stat(keep); err != nil {
		t.Errorf("fixed staging dir must survive the sweep: %v", err)
	}
	for _, d := range stale {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("stale dir %s still present (err=%v)", d, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated cache dir must survive: %v", err)
	}
}

// An empty cache must not error or create anything.
func TestSweepStaleStagingOnEmptyCache(t *testing.T) {
	cache := t.TempDir()
	sweepStaleStaging(cache)
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("sweep created %d entries, want none", len(entries))
	}
}
