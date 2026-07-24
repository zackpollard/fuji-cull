package main

import (
	"path/filepath"
	"testing"

	"github.com/zack/fuji-tools/internal/synccore"
)

func newStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dec(ck, d string, wall int64, dev string) synccore.DecisionRow {
	return synccore.DecisionRow{Ckey: ck, D: d, HLC: synccore.HLC{Wall: wall, Dev: dev}}
}

func TestPushAcceptAndVersion(t *testing.T) {
	s := newStore(t)
	resp := s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("151_FUJI/DSCF0001", "keep", 1000, "A")}}, 5000)
	if len(resp.Results) != 1 || !resp.Results[0].Accepted || resp.Results[0].Version != 1 {
		t.Fatalf("expected accepted v1, got %+v", resp.Results)
	}
	if resp.CameraVersion != 1 {
		t.Errorf("cameraVersion = %d, want 1", resp.CameraVersion)
	}
}

func TestPushRejectOlderReturnsWinner(t *testing.T) {
	s := newStore(t)
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "keep", 2000, "A")}}, 5000)
	// older wall -> loses; server returns the stored winner
	resp := s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "reject", 1000, "B")}}, 5001)
	if resp.Results[0].Accepted {
		t.Fatal("older push should be rejected")
	}
	if resp.Results[0].Winner.D != "keep" {
		t.Errorf("rejected push should return the winning 'keep', got %q", resp.Results[0].Winner.D)
	}
}

func TestPushNewerWins(t *testing.T) {
	s := newStore(t)
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "keep", 1000, "A")}}, 5000)
	resp := s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "reject", 2000, "B")}}, 5001)
	if !resp.Results[0].Accepted || resp.Results[0].Winner.D != "reject" {
		t.Errorf("newer push should win, got %+v", resp.Results[0])
	}
}

func TestMigratedLosesToGenuineServer(t *testing.T) {
	s := newStore(t)
	// migrated seed with a FUTURE wall
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{
		{Ckey: "k", D: "keep", HLC: synccore.HLC{Wall: 9_000_000, Dev: "A"}, Migrated: true},
	}}, 5000)
	// genuine edit with an OLDER wall must still win
	resp := s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "reject", 1000, "B")}}, 5001)
	if !resp.Results[0].Accepted || resp.Results[0].Winner.D != "reject" {
		t.Errorf("genuine should beat migrated on the server, got %+v", resp.Results[0])
	}
}

func TestPullSince(t *testing.T) {
	s := newStore(t)
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("a", "keep", 1000, "A")}}, 5000)
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("b", "reject", 1001, "A")}}, 5001)

	full := s.pull("cam", 0, 6000)
	if len(full.Decisions) != 2 || full.DeltaHigh != 2 {
		t.Fatalf("pull since 0 = %d rows, deltaHigh %d", len(full.Decisions), full.DeltaHigh)
	}
	// pull since the high-water -> nothing new
	empty := s.pull("cam", full.DeltaHigh, 6001)
	if len(empty.Decisions) != 0 {
		t.Errorf("pull since deltaHigh should be empty, got %v", empty.Decisions)
	}
	// pull since 1 -> just the second row
	partial := s.pull("cam", 1, 6002)
	if len(partial.Decisions) != 1 || partial.Decisions[0].Ckey != "b" {
		t.Errorf("pull since 1 = %+v", partial.Decisions)
	}
}

func TestPersistenceAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	s, _ := openStore(path)
	epoch := s.data.Epoch
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "keep", 1000, "A")}}, 5000)

	s2, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.data.Epoch != epoch {
		t.Errorf("epoch changed on a clean reopen: %q -> %q", epoch, s2.data.Epoch)
	}
	got := s2.pull("cam", 0, 6000)
	if len(got.Decisions) != 1 || got.Decisions[0].D != "keep" {
		t.Errorf("data did not persist: %+v", got.Decisions)
	}
}

func TestGenerationGuardOnRewind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	s, _ := openStore(path)
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "keep", 1000, "A")}}, 5000)
	origEpoch := s.data.Epoch

	// simulate a restored-stale DB: HighVersionEver preserved but camera rewound
	s.data.Cameras["cam"].Version = 0
	s.data.Cameras["cam"].Decisions = map[string]*decisionRow{}
	_ = s.save()

	s2, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.data.Epoch == origEpoch {
		t.Error("rewound store should mint a fresh epoch to force client re-seed")
	}
}

func TestContestedFlag(t *testing.T) {
	s := newStore(t)
	// two genuine, different-device edits with close walls and a value change
	s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "keep", 1000, "A")}}, 5000)
	resp := s.push(synccore.PushRequest{Camera: "cam", Decisions: []synccore.DecisionRow{dec("k", "reject", 1500, "B")}}, 5001)
	if !resp.Results[0].Contested {
		t.Error("a close, different-device value change should be flagged contested")
	}
}
