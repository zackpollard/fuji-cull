package cull

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

func tmpSession(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "s.json")
}

// writeV1 writes a legacy (pre-v2) session file, like the real iPad's.
func writeV1(t *testing.T, path string, decisions map[string]string, updated time.Time) {
	t.Helper()
	raw, _ := json.MarshalIndent(map[string]any{
		"decisions": decisions,
		"cursor":    0,
		"updatedAt": updated,
	}, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Migration derives Records from a real-format v1 file and leaves Decisions
// serving the original keys byte-for-byte.
func TestMigrateV1PreservesDecisions(t *testing.T) {
	p := tmpSession(t)
	writeV1(t, p, map[string]string{
		"SLOT 1/DCIM/151_FUJI/DSCF8558": "keep",
		"SLOT 1/DCIM/151_FUJI/DSCF8560": "reject",
	}, time.Date(2026, 7, 23, 23, 26, 36, 0, time.UTC))

	s, err := loadSession(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.data.Version != 2 {
		t.Errorf("version = %d, want 2", s.data.Version)
	}
	// legacy readers unchanged
	dec := s.Decisions()
	if dec["SLOT 1/DCIM/151_FUJI/DSCF8558"] != "keep" || dec["SLOT 1/DCIM/151_FUJI/DSCF8560"] != "reject" {
		t.Errorf("Decisions projection changed: %v", dec)
	}
	// records derived, canonical-keyed, migrated
	r, ok := s.data.Records["151_FUJI/DSCF8558"]
	if !ok || r.D != "keep" || !r.Migrated {
		t.Errorf("record for canonical key missing/wrong: %+v", r)
	}
	if r.HLC.Wall != time.Date(2026, 7, 23, 23, 26, 36, 0, time.UTC).UnixMilli() {
		t.Errorf("migrated HLC wall should be the file UpdatedAt, got %d", r.HLC.Wall)
	}
}

// Two legacy keys (pre/post PTP-fix) for the same frame collapse deterministically;
// reject wins a true conflict.
func TestMigrateN1Collapse(t *testing.T) {
	p := tmpSession(t)
	writeV1(t, p, map[string]string{
		"SLOT 1/DCIM/151_FUJI/DSCF0001": "keep",   // objects-fallback shape
		"DCIM/151_FUJI/DSCF0001":        "reject", // PTP shape, same frame
	}, time.Now())
	s, _ := loadSession(p)
	if len(s.data.Records) != 1 {
		t.Fatalf("expected 1 collapsed record, got %d: %v", len(s.data.Records), s.data.Records)
	}
	if r := s.data.Records["151_FUJI/DSCF0001"]; r.D != "reject" {
		t.Errorf("conflict should resolve to reject, got %q", r.D)
	}
}

// A cleared decision is a tombstone, not a key delete — so a stale older keep
// can't resurrect it.
func TestClearIsTombstone(t *testing.T) {
	p := tmpSession(t)
	s, _ := loadSession(p)
	s.SetResolvers(
		map[string]string{"SLOT 1/DCIM/151_FUJI/DSCF0001": "151_FUJI/DSCF0001"},
		map[string][]string{"151_FUJI/DSCF0001": {"SLOT 1/DCIM/151_FUJI/DSCF0001"}},
	)
	s.SetDecision("SLOT 1/DCIM/151_FUJI/DSCF0001", "keep")
	s.SetDecision("SLOT 1/DCIM/151_FUJI/DSCF0001", "") // clear
	r, ok := s.data.Records["151_FUJI/DSCF0001"]
	if !ok || !r.Del {
		t.Fatalf("clear should leave a tombstone, got %+v (ok=%v)", r, ok)
	}
	if _, present := s.Decisions()["SLOT 1/DCIM/151_FUJI/DSCF0001"]; present {
		t.Errorf("cleared decision should not project into Decisions")
	}
	// a remote keep with an OLDER hlc must not win over the tombstone
	older := hlc{Wall: r.HLC.Wall - 1000, Ctr: 0, Dev: "other"}
	s.ApplyRemote([]synccore.DecisionRow{{Ckey: "151_FUJI/DSCF0001", D: "keep", HLC: older}}, nil, "", 0, 0)
	if r2 := s.data.Records["151_FUJI/DSCF0001"]; !r2.Del {
		t.Errorf("older remote keep resurrected a tombstone: %+v", r2)
	}
}

// A genuine (non-migrated) remote record always beats a migrated local one,
// regardless of wall clock.
func TestMigratedLosesToGenuine(t *testing.T) {
	p := tmpSession(t)
	writeV1(t, p, map[string]string{"151_FUJI/DSCF0001": "keep"}, time.Now().Add(48*time.Hour)) // future-dated migrated
	s, _ := loadSession(p)
	s.SetResolvers(map[string]string{"151_FUJI/DSCF0001": "151_FUJI/DSCF0001"},
		map[string][]string{"151_FUJI/DSCF0001": {"151_FUJI/DSCF0001"}})

	// remote reject with an OLDER wall but non-migrated -> must win
	s.ApplyRemote([]synccore.DecisionRow{{
		Ckey: "151_FUJI/DSCF0001", D: "reject",
		HLC: hlc{Wall: 1000, Ctr: 0, Dev: "srv"}, Migrated: false, Version: 5,
	}}, nil, "e1", 5, nowMs())
	if r := s.data.Records["151_FUJI/DSCF0001"]; r.D != "reject" || r.Migrated {
		t.Errorf("genuine remote should beat migrated, got %+v", r)
	}
	if s.Decisions()["151_FUJI/DSCF0001"] != "reject" {
		t.Errorf("projection not updated after remote win: %v", s.Decisions())
	}
}

// ApplyRemote is idempotent: re-applying the same batch changes nothing.
func TestApplyRemoteIdempotent(t *testing.T) {
	p := tmpSession(t)
	s, _ := loadSession(p)
	s.SetResolvers(map[string]string{}, map[string][]string{"151_FUJI/DSCF0001": {"L/151_FUJI/DSCF0001"}})
	batch := []synccore.DecisionRow{{Ckey: "151_FUJI/DSCF0001", D: "keep", HLC: hlc{Wall: 5000, Dev: "srv"}, Version: 3}}
	if !s.ApplyRemote(batch, nil, "e", 3, nowMs()) {
		t.Fatal("first apply should change state")
	}
	if s.ApplyRemote(batch, nil, "e", 3, nowMs()) {
		t.Error("second identical apply should be a no-op")
	}
}

// The outbox is the unacked (SV==0) set; AckPush cleans a record, but a re-edit
// after dispatch keeps it dirty.
func TestOutboxAndAck(t *testing.T) {
	p := tmpSession(t)
	s, _ := loadSession(p)
	s.SetResolvers(map[string]string{"L/151_FUJI/DSCF0001": "151_FUJI/DSCF0001"},
		map[string][]string{"151_FUJI/DSCF0001": {"L/151_FUJI/DSCF0001"}})
	s.SetDecision("L/151_FUJI/DSCF0001", "keep")

	out := s.Outbox()
	if len(out) != 1 || out[0].Ckey != "151_FUJI/DSCF0001" {
		t.Fatalf("outbox = %v", out)
	}
	sent := out[0]
	// ack with the same winner -> cleaned
	s.AckPush(synccore.PushResponse{Results: []synccore.AckRow{{Ckey: sent.Ckey, Accepted: true, Version: 10, Winner: synccore.DecisionRow{D: "keep", HLC: sent.HLC}}}})
	if len(s.Outbox()) != 0 {
		t.Errorf("record should be clean after matching ack, outbox=%v", s.Outbox())
	}
	// re-edit, dispatch, then ack the OLD winner -> stays dirty (we moved on)
	s.SetDecision("L/151_FUJI/DSCF0001", "reject")
	s.AckPush(synccore.PushResponse{Results: []synccore.AckRow{{Ckey: sent.Ckey, Accepted: true, Version: 11, Winner: synccore.DecisionRow{D: "keep", HLC: sent.HLC}}}})
	if len(s.Outbox()) != 1 {
		t.Errorf("re-edited record should remain dirty until its own ack, outbox=%v", s.Outbox())
	}
}

// Records applied before the resolver is installed project correctly once
// SetResolvers runs (the finishInit re-projection sweep).
func TestReprojectAfterResolvers(t *testing.T) {
	p := tmpSession(t)
	s, _ := loadSession(p)
	// no resolver yet (pre-discovery): apply a remote decision
	s.ApplyRemote([]synccore.DecisionRow{{Ckey: "151_FUJI/DSCF0001", D: "keep", HLC: hlc{Wall: 9000, Dev: "srv"}, Version: 1}}, nil, "e", 1, nowMs())
	if len(s.Decisions()) != 0 {
		t.Errorf("no projection expected before resolver, got %v", s.Decisions())
	}
	// discovery installs the resolver -> reproject
	s.SetResolvers(map[string]string{"SLOT 1/DCIM/151_FUJI/DSCF0001": "151_FUJI/DSCF0001"},
		map[string][]string{"151_FUJI/DSCF0001": {"SLOT 1/DCIM/151_FUJI/DSCF0001"}})
	if s.Decisions()["SLOT 1/DCIM/151_FUJI/DSCF0001"] != "keep" {
		t.Errorf("record not projected after SetResolvers: %v", s.Decisions())
	}
}

// Round-trips: save then reload preserves the v2 store and does not re-migrate.
func TestV2RoundTrip(t *testing.T) {
	p := tmpSession(t)
	s, _ := loadSession(p)
	s.SetResolvers(map[string]string{"L/151_FUJI/DSCF0001": "151_FUJI/DSCF0001"},
		map[string][]string{"151_FUJI/DSCF0001": {"L/151_FUJI/DSCF0001"}})
	s.SetDecision("L/151_FUJI/DSCF0001", "keep")
	dev := s.DeviceID()

	s2, err := loadSession(p)
	if err != nil {
		t.Fatal(err)
	}
	if s2.DeviceID() != dev {
		t.Errorf("deviceID not stable: %q vs %q", s2.DeviceID(), dev)
	}
	r := s2.data.Records["151_FUJI/DSCF0001"]
	if r.D != "keep" || r.Migrated {
		t.Errorf("reloaded record wrong: %+v", r)
	}
}

// A backup twin: a remote decision on the shared canonical key projects onto BOTH
// local slot IDs.
func TestRemoteProjectsToBothTwins(t *testing.T) {
	p := tmpSession(t)
	s, _ := loadSession(p)
	s.SetResolvers(
		map[string]string{"SLOT 1/DCIM/151_FUJI/DSCF0001": "151_FUJI/DSCF0001", "SLOT 2/DCIM/151_FUJI/DSCF0001": "151_FUJI/DSCF0001"},
		map[string][]string{"151_FUJI/DSCF0001": {"SLOT 1/DCIM/151_FUJI/DSCF0001", "SLOT 2/DCIM/151_FUJI/DSCF0001"}},
	)
	s.ApplyRemote([]synccore.DecisionRow{{Ckey: "151_FUJI/DSCF0001", D: "keep", HLC: hlc{Wall: 5000, Dev: "srv"}, Version: 2}}, nil, "e", 2, nowMs())
	dec := s.Decisions()
	if dec["SLOT 1/DCIM/151_FUJI/DSCF0001"] != "keep" || dec["SLOT 2/DCIM/151_FUJI/DSCF0001"] != "keep" {
		t.Errorf("remote decision should project to both twins: %v", dec)
	}
}
