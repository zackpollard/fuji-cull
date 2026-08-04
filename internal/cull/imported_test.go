package cull

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The workflow this exists for: cull event A, import it, then cull event B.
// The second import must carry only B — otherwise it re-pulls A from the
// camera and files all of it into B's album.
func TestImportedShotsLeaveTheQueue(t *testing.T) {
	dir := t.TempDir()
	s, err := loadSession(filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkImported([]string{"151_FUJI/DSCF0001", "151_FUJI/DSCF0002"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := s.ImportedKeys()
	if len(got) != 2 {
		t.Fatalf("recorded %d imported keys, want 2", len(got))
	}
	if got["151_FUJI/DSCF0001"] == "" {
		t.Error("imported marker has no timestamp")
	}

	// survives a reload — the next launch must still know
	s2, err := loadSession(filepath.Join(dir, "s.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.ImportedKeys()) != 2 {
		t.Errorf("imported markers did not persist: %d", len(s2.ImportedKeys()))
	}

	// clearing puts them back, and must not touch decisions
	if err := s2.SetDecision("151_FUJI/DSCF0001", "keep"); err != nil {
		t.Fatal(err)
	}
	n, err := s2.ClearImported()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("cleared %d, want 2", n)
	}
	if len(s2.ImportedKeys()) != 0 {
		t.Error("markers survived a clear")
	}
	if d := s2.Decisions()["151_FUJI/DSCF0001"]; d != "keep" {
		t.Errorf("clearing imported markers destroyed a decision: %q", d)
	}
	if _, err := os.Stat(filepath.Join(dir, "s.json")); err != nil {
		t.Error("session file missing after clear")
	}
}
