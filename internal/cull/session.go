package cull

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

// Session persists culling decisions so a run survives disconnects/restarts, and
// (v2) is the local source of truth + durable outbox for cross-device sync.
//
// The authoritative store is Records: one HLC-LWW register per DEVICE-INDEPENDENT
// canonical key (canonical.go). The legacy Decisions map (keyed by backend-local
// shot ID) is kept as a PROJECTION of Records so every existing reader —
// Decisions(), /api/state, /api/status, the clients — is byte-for-byte unchanged.
// Saved atomically on every mutation (the file is tiny); an unacked record
// (SV==0) IS the outbox entry, so no separate outbox file and no two-write gap.
type Session struct {
	mu   sync.Mutex
	path string
	data sessionData

	// resolvers bridge legacy shot IDs and canonical keys; injected once the
	// catalog exists (SetResolvers at finishInit). Nil before discovery — writes
	// then fall back to string canonicalization and projection is deferred.
	canonical map[string]string   // legacyID -> canonicalKey (outbound)
	legacy    map[string][]string // canonicalKey -> []legacyID (inbound projection)

	serverRef int64  // last server clock seen (ms); clamps locally-minted walls forward. 0 = none
	onDirty    func() // woken after a local mutation leaves an unacked record
}

type sessionData struct {
	Version   int    `json:"version"`             // 2; absent/0 => v1 on-disk, migrated on load
	DeviceID  string `json:"deviceId"`            // stable per-install UUID, minted once
	NodeHLC   hlc    `json:"nodeHlc"`             // persisted node clock (step-back safe)
	Camera    string `json:"camera,omitempty"`    // identity slug, so we can sync with no camera attached
	ServerVer int64  `json:"serverVer,omitempty"` // pull high-water; advanced ONLY by a pull
	Epoch     string `json:"epoch,omitempty"`     // last-seen server generation token

	Decisions map[string]string `json:"decisions"` // LEGACY projection, key=shot ID — kept populated for all readers + downgrade
	Cursor    int               `json:"cursor"`    // LEGACY local int index — tag/type UNCHANGED
	UpdatedAt time.Time         `json:"updatedAt"` // whole-file, unchanged

	Records map[string]record    `json:"records,omitempty"` // key=canonicalKey — sync source of truth
	Resume  map[string]cursorRec `json:"resume,omitempty"`  // key=deviceId — per-device resume points
}

// record is one HLC-LWW register: a keep/reject decision or a tombstone.
type record struct {
	D        string `json:"d"`             // "keep" | "reject" (ignored when Del)
	Del      bool   `json:"del,omitempty"` // tombstone: an explicitly cleared decision
	HLC      hlc    `json:"h"`             // ordering clock
	SV       int64  `json:"sv,omitempty"`  // server version once acked; 0 == dirty/unacked == the outbox flag
	Migrated bool   `json:"m,omitempty"`   // seeded from a v1 file; loses to any genuine post-v2 edit
}

type cursorRec struct {
	K   string `json:"k"` // canonicalKey resume point
	HLC hlc    `json:"h"`
	SV  int64  `json:"sv,omitempty"`
}

func loadSession(path string) (*Session, error) {
	s := &Session{path: path, data: sessionData{Decisions: map[string]string{}}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.initV2Locked()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", path, err)
	}
	if s.data.Decisions == nil {
		s.data.Decisions = map[string]string{}
	}
	// Persist the one-time v1->v2 upgrade (mint deviceID, derive Records) so the
	// deviceID is stable across restarts and we never re-migrate.
	if s.initV2Locked() {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// initV2Locked upgrades an in-memory sessionData to v2: mint a deviceID, ensure
// maps, migrate v1 decisions into Records, and seed the node clock. Idempotent.
// Returns true if a durable upgrade happened (deviceID minted or v1 migrated),
// meaning the caller should persist.
func (s *Session) initV2Locked() (upgraded bool) {
	if s.data.DeviceID == "" {
		s.data.DeviceID = newUUID()
		upgraded = true
	}
	if s.data.Records == nil {
		s.data.Records = map[string]record{}
	}
	if s.data.Resume == nil {
		s.data.Resume = map[string]cursorRec{}
	}
	if s.data.Version < 2 {
		s.migrateV1Locked()
		s.data.Version = 2
		upgraded = true
	}
	// seed the node clock to the max over everything we hold, so a wall-clock
	// step-back across a restart can't rewind causality (FIX-27)
	seed := s.data.NodeHLC
	seed.Wall = maxInt64(seed.Wall, nowMs())
	for _, r := range s.data.Records {
		seed = maxHLC(seed, r.HLC)
	}
	for _, c := range s.data.Resume {
		seed = maxHLC(seed, c.HLC)
	}
	s.data.NodeHLC = hlc{Wall: seed.Wall, Ctr: seed.Ctr, Dev: s.data.DeviceID}
	return upgraded
}

// migrateV1Locked derives Records from the legacy Decisions map by parsing each
// key (no catalog needed). Migrated records get the file's UpdatedAt as their HLC
// wall and Migrated:true, so they only ever fill gaps and lose to a genuine
// post-v2 edit. N:1 collapse (the same frame under pre/post PTP-fix keys) is
// resolved deterministically (reject>keep on a true conflict) and logged.
func (s *Session) migrateV1Locked() {
	if len(s.data.Decisions) == 0 {
		return
	}
	wall := s.data.UpdatedAt.UnixMilli()
	if wall <= 0 {
		wall = 1 // low sentinel — still strictly below any real post-v2 edit
	}
	// group legacy ids by canonical key, deterministic order
	byCanon := map[string][]string{} // ckey -> []legacyID
	for id := range s.data.Decisions {
		ck, ok := canonicalizeLegacyKey(id)
		if !ok {
			ck = id // non-Fuji key: keep as its own canonical (round-trips in projection)
		}
		byCanon[ck] = append(byCanon[ck], id)
	}
	for ck, ids := range byCanon {
		sort.Strings(ids)
		// safety-biased collapse: reject beats keep on a true value conflict
		val, conflict := s.data.Decisions[ids[0]], false
		for _, id := range ids[1:] {
			if s.data.Decisions[id] != val {
				conflict = true
			}
			if s.data.Decisions[id] == "reject" {
				val = "reject"
			}
		}
		if conflict {
			log.Printf("sync: migration collapsed %d legacy keys onto %q -> %q (had a conflict; reject wins)", len(ids), ck, val)
		}
		s.data.Records[ck] = record{
			D:        val,
			HLC:      hlc{Wall: wall, Ctr: 0, Dev: s.data.DeviceID},
			Migrated: true,
			SV:       0, // seeds the server on first sync
		}
	}
}

// SetResolvers installs the catalog bridges and re-projects Records into the
// legacy Decisions map (covers records applied before discovery). Call once the
// catalog exists.
func (s *Session) SetResolvers(canonical map[string]string, legacy map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canonical = canonical
	s.legacy = legacy
	s.reprojectLocked()
	_ = s.saveLocked()
}

// reprojectLocked rebuilds the legacy Decisions map from Records using the
// resolver. A record whose canonical key maps to no local shot projects to
// nothing (a remote decision for a frame not on this card) unless the key is a
// raw non-canonical id, which round-trips to itself.
func (s *Session) reprojectLocked() {
	next := make(map[string]string, len(s.data.Decisions))
	for ck, r := range s.data.Records {
		if r.Del {
			continue
		}
		ids := s.legacy[ck]
		if len(ids) == 0 {
			if _, canon := canonicalizeLegacyKey(ck); !canon {
				next[ck] = r.D // raw legacy id with no catalog mapping — preserve it
			}
			continue
		}
		for _, id := range ids {
			next[id] = r.D
		}
	}
	s.data.Decisions = next
}

func (s *Session) saveLocked() error {
	s.data.UpdatedAt = time.Now()
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// canonicalForLocked maps a backend-local shot ID to its canonical key: via the
// resolver when the catalog is present, else by string parsing, else the id
// verbatim (a non-Fuji key becomes its own canonical).
func (s *Session) canonicalForLocked(id string) string {
	if s.canonical != nil {
		if ck, ok := s.canonical[id]; ok && ck != "" {
			return ck
		}
	}
	if ck, ok := canonicalizeLegacyKey(id); ok {
		return ck
	}
	return id
}

// legacyIDsForLocked returns every local shot ID sharing a canonical key (for
// projection). Falls back to the id the caller acted on when no resolver is set.
func (s *Session) legacyIDsForLocked(ck, actedID string) []string {
	if ids := s.legacy[ck]; len(ids) > 0 {
		return ids
	}
	if actedID != "" {
		return []string{actedID}
	}
	return nil
}

// SetDecision records a local decision; decision "" clears it (a tombstone).
func (s *Session) SetDecision(id, decision string) error {
	s.mu.Lock()
	ck := s.canonicalForLocked(id)
	r := record{HLC: s.nextHLCLocked(), SV: 0}
	if decision == "" {
		r.Del = true
	} else {
		r.D = decision
	}
	s.data.Records[ck] = r
	s.projectLocked(ck, r, id)
	err := s.saveLocked()
	cb := s.onDirty
	s.mu.Unlock()
	if cb != nil {
		cb() // woken outside the lock; onDirty read under it, so no race
	}
	return err
}

// SetOnDirty installs the callback woken after a local mutation leaves an unacked
// record (the syncer's Nudge). Safe against concurrent SetDecision.
func (s *Session) SetOnDirty(f func()) {
	s.mu.Lock()
	s.onDirty = f
	s.mu.Unlock()
}

// projectLocked writes a record's effect into the legacy Decisions map for every
// local shot sharing the canonical key (actedID is the fallback used before the
// catalog resolver is installed).
func (s *Session) projectLocked(ck string, r record, actedID string) {
	for _, lid := range s.legacyIDsForLocked(ck, actedID) {
		if r.Del {
			delete(s.data.Decisions, lid)
		} else {
			s.data.Decisions[lid] = r.D
		}
	}
}

// nextHLCLocked advances and persists the node clock and returns the next stamp,
// clamped forward to the last server clock seen (+24h) so a runaway local RTC
// can't pin the node clock in the future.
func (s *Session) nextHLCLocked() hlc {
	var ceil int64
	if s.serverRef > 0 {
		ceil = s.serverRef + 24*3600*1000
	}
	stamp, next := tickHLC(s.data.NodeHLC, s.data.DeviceID, nowMs(), ceil)
	s.data.NodeHLC = next
	return stamp
}

func (s *Session) SetCursor(i int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Cursor == i {
		return nil
	}
	s.data.Cursor = i
	return s.saveLocked()
}

func (s *Session) Cursor() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Cursor
}

// Decisions returns a copy of the legacy decisions map (the projection).
func (s *Session) Decisions() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data.Decisions))
	for k, v := range s.data.Decisions {
		out[k] = v
	}
	return out
}

// DeviceID returns this install's stable sync id.
func (s *Session) DeviceID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.DeviceID
}

// recordWins reports whether `in` should replace `stored` under the shared
// HLC-LWW rule (synccore is the single merge authority for engine and server).
func recordWins(in, stored record, storedExists bool) bool {
	return synccore.Wins(in.HLC, in.Migrated, stored.HLC, stored.Migrated, storedExists)
}

// helper for int64 max
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
