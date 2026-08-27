package cull

import (
	"os"
	"testing"
)

// killAndReload simulates the process dying without a clean shutdown — which
// is exactly how the desktop GUI exits — by dropping the Session without
// closing it and loading the file again.
func killAndReload(t *testing.T, path string) *Session {
	t.Helper()
	s, err := loadSession(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return s
}

// The whole point of the journal: a decision is durable the moment it is made,
// with no snapshot and no clean shutdown in between.
func TestDecisionSurvivesUncleanExit(t *testing.T) {
	path := t.TempDir() + "/s.json"
	s, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("151_FUJI/DSCF0001", "reject"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("151_FUJI/DSCF0002", "keep"); err != nil {
		t.Fatal(err)
	}
	// no Close: the process "dies" here

	got := killAndReload(t, path).Decisions()
	if got["151_FUJI/DSCF0001"] != "reject" || got["151_FUJI/DSCF0002"] != "keep" {
		t.Errorf("decisions after an unclean exit = %v, want both recorded", got)
	}
}

// A cleared decision is a tombstone, and must survive the same way.
func TestClearedDecisionSurvivesUncleanExit(t *testing.T) {
	path := t.TempDir() + "/s.json"
	s, _ := loadSession(path)
	if err := s.SetDecision("151_FUJI/DSCF0003", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("151_FUJI/DSCF0003", ""); err != nil {
		t.Fatal(err)
	}

	if got := killAndReload(t, path).Decisions(); got["151_FUJI/DSCF0003"] != "" {
		t.Errorf("cleared decision came back as %q", got["151_FUJI/DSCF0003"])
	}
}

// Taking a snapshot must not drop a decision, and must leave the journal
// retired rather than growing forever.
func TestSnapshotRetiresJournal(t *testing.T) {
	path := t.TempDir() + "/s.json"
	s, _ := loadSession(path)
	if err := s.SetDecision("151_FUJI/DSCF0004", "reject"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + journalSuffix); err != nil {
		t.Fatalf("no journal after a decision: %v", err)
	}
	s.mu.Lock()
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + journalSuffix); !os.IsNotExist(err) {
		t.Errorf("journal still present after a snapshot (err=%v)", err)
	}
	if got := killAndReload(t, path).Decisions(); got["151_FUJI/DSCF0004"] != "reject" {
		t.Errorf("decision lost across the snapshot: %v", got)
	}
}

// Replaying entries the snapshot already holds happens whenever a crash lands
// between writing the snapshot and retiring the journal. It must be a no-op,
// not a corruption.
func TestReplayIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/s.json"
	s, _ := loadSession(path)
	if err := s.SetDecision("151_FUJI/DSCF0005", "keep"); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(path + journalSuffix)
	if err != nil {
		t.Fatal(err)
	}
	s.Close() // snapshot written, journal retired

	// put the journal back, as a crash before the retire would leave it
	if err := os.WriteFile(path+journalSuffix, journal, 0o644); err != nil {
		t.Fatal(err)
	}
	got := killAndReload(t, path).Decisions()
	if got["151_FUJI/DSCF0005"] != "keep" {
		t.Errorf("decisions = %v, want the keep intact", got)
	}
	if len(got) != 1 {
		t.Errorf("replay duplicated records: %v", got)
	}
}

// Being killed mid-append leaves a half-written last line. Everything written
// before it is still good and must be recovered.
func TestTornFinalLineKeepsEarlierDecisions(t *testing.T) {
	path := t.TempDir() + "/s.json"
	s, _ := loadSession(path)
	if err := s.SetDecision("151_FUJI/DSCF0006", "reject"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("151_FUJI/DSCF0007", "keep"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path + journalSuffix)
	if err != nil {
		t.Fatal(err)
	}
	// lop the tail off the final entry
	if err := os.WriteFile(path+journalSuffix, raw[:len(raw)-12], 0o644); err != nil {
		t.Fatal(err)
	}
	got := killAndReload(t, path).Decisions()
	if got["151_FUJI/DSCF0006"] != "reject" {
		t.Errorf("a torn final line lost an earlier decision: %v", got)
	}
}

// The cursor is deliberately not journalled — it runs on the render thread and
// losing it costs a scroll — but a clean stop still records it.
func TestCursorPersistsOnClose(t *testing.T) {
	path := t.TempDir() + "/s.json"
	s, _ := loadSession(path)
	if err := s.SetCursor(4242); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := killAndReload(t, path).Cursor(); got != 4242 {
		t.Errorf("cursor after a clean stop = %d, want 4242", got)
	}
}
