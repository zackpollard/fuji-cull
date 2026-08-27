package cull

import (
	"bufio"
	"bytes"
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
	onDirty   func() // woken after a local mutation leaves an unacked record

	// Journal state. The snapshot is rewritten on a timer; between snapshots
	// the journal is what makes a decision durable. See journalEntry.
	jf        *os.File // append handle, opened on first write
	dirty     bool     // snapshot is behind the in-memory data
	stop      chan struct{}
	closeOnce sync.Once
}

// The session file is the only durable record of a cull: sync records double as
// the outbox, there is no server configured by default, and the desktop GUI
// never gets a clean shutdown. So a decision has to survive the process dying
// the instant it is made. Rewriting the whole file to achieve that cost 28 ms
// of held lock per keypress on a 16,000-record card — hold a key down and saves
// queue faster than they complete, and SetCursor was doing it on the render
// thread. Decisions are therefore appended to a journal (one short line, one
// write) and the full snapshot is rewritten on a timer. Snapshot plus journal
// is always the complete history: a crash replays what the snapshot missed.
const (
	snapshotInterval = 3 * time.Second
	journalSuffix    = ".journal"
)

// journalEntry is one durable mutation. It carries the acted-on shot ID as well
// as the canonical key because the legacy projection needs it whenever the
// catalog resolver is absent — which is exactly the situation during a replay.
type journalEntry struct {
	CK  string `json:"ck"`
	ID  string `json:"id,omitempty"`
	Rec record `json:"r"`
}

func (s *Session) journalPath() string { return s.path + journalSuffix }

// appendJournalLocked makes one record durable without rewriting the file.
// Deliberately no fsync: this guards against the process dying, which is what
// actually happens here, and the whole-file write it replaces never fsynced
// either — so durability is unchanged, only the cost is.
func (s *Session) appendJournalLocked(ck, actedID string, r record) error {
	if s.jf == nil {
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(s.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		s.jf = f
	}
	line, err := json.Marshal(journalEntry{CK: ck, ID: actedID, Rec: r})
	if err != nil {
		return err
	}
	_, err = s.jf.Write(append(line, '\n'))
	return err
}

// replayJournalLocked applies decisions journalled since the last snapshot.
// Re-applying one the snapshot already holds is harmless — same key, same
// value — and that idempotence is what makes discarding the journal after a
// snapshot safe.
func (s *Session) replayJournalLocked() (int, error) {
	f, err := os.Open(s.journalPath())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// A half-written final line is what being killed mid-append looks
			// like. Everything before it is still good, so take that and go on.
			log.Printf("session: journal: discarding an unreadable entry (%v)", err)
			continue
		}
		if e.CK == "" {
			continue
		}
		if s.data.Records == nil {
			s.data.Records = map[string]record{}
		}
		s.data.Records[e.CK] = e.Rec
		s.projectLocked(e.CK, e.Rec, e.ID)
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	if n > 0 {
		log.Printf("session: recovered %d decisions from the journal", n)
	}
	return n, nil
}

// snapshotLoop rewrites the full file in the background so the hot path never
// has to.
func (s *Session) snapshotLoop() {
	t := time.NewTicker(snapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			if s.dirty {
				if err := s.saveLocked(); err != nil {
					log.Printf("session: snapshot: %v", err)
				}
			}
			s.mu.Unlock()
		}
	}
}

// Close stops the background writer after a final snapshot. Correctness does
// not depend on it — the journal already holds anything the snapshot missed —
// it just leaves the file tidy.
func (s *Session) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		if err := s.saveLocked(); err != nil {
			log.Printf("session: final snapshot: %v", err)
		}
	}
	if s.jf != nil {
		s.jf.Close()
		s.jf = nil
	}
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

	// Imported records shots this session has already imported, key=canonicalKey,
	// value=RFC3339 of the run. "keep" means "pending import", so a finished
	// event drops out of the queue and the next one starts clean instead of
	// re-sending everything — and, worse, filing it all into the new album.
	// Canonical keys because it must survive a re-index, like Records.
	Imported map[string]string `json:"imported,omitempty"`
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
	s := &Session{
		path: path,
		data: sessionData{Decisions: map[string]string{}},
		stop: make(chan struct{}),
	}
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		s.initV2Locked()
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("parse session %s: %w", path, err)
		}
		if s.data.Decisions == nil {
			s.data.Decisions = map[string]string{}
		}
		// The one-time v1->v2 upgrade (mint deviceID, derive Records) runs
		// before the replay so a journalled record lands on a v2 shape.
		s.initV2Locked()
	}
	// Anything decided after the last snapshot lives here, including the
	// decisions of a run that was killed rather than closed.
	if _, err := s.replayJournalLocked(); err != nil {
		return nil, fmt.Errorf("replay session journal %s: %w", s.journalPath(), err)
	}
	// One snapshot at startup makes the file self-contained again: the deviceID
	// is stable across restarts, a migration never repeats, and the journal
	// starts empty.
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	go s.snapshotLoop()
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
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// The snapshot now holds everything the journal did. Retiring it under the
	// same lock that marshalled the data is what makes that true: no decision
	// can land in between and be dropped.
	s.dirty = false
	if s.jf != nil {
		s.jf.Close()
		s.jf = nil
	}
	if err := os.Remove(s.journalPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("session: could not retire the journal: %v", err)
	}
	return nil
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
	err := s.appendJournalLocked(ck, id, r)
	if err != nil {
		// Never lose a decision to a journal problem: fall back to the
		// whole-file write, which is what this always used to do.
		log.Printf("session: journal append failed (%v) — writing the full snapshot", err)
		err = s.saveLocked()
	} else {
		s.dirty = true
	}
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
	// Deliberately not persisted here. Losing the cursor to a crash costs the
	// user one scroll, which is not worth rewriting the whole file for on every
	// arrow key — and this runs on the GUI's render thread.
	s.dirty = true
	return nil
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

// MarkImported records canonical keys as imported, stamped with when.
func (s *Session) MarkImported(keys []string, when time.Time) error {
	if len(keys) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Imported == nil {
		s.data.Imported = map[string]string{}
	}
	stamp := when.UTC().Format(time.RFC3339)
	for _, k := range keys {
		if k != "" {
			s.data.Imported[k] = stamp
		}
	}
	return s.saveLocked()
}

// ImportedKeys returns the canonical keys already imported.
func (s *Session) ImportedKeys() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data.Imported))
	for k, v := range s.data.Imported {
		out[k] = v
	}
	return out
}

// ClearImported forgets every imported marker, returning how many were dropped.
// Decisions are untouched: this only puts the keepers back in the queue.
func (s *Session) ClearImported() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.data.Imported)
	if n == 0 {
		return 0, nil
	}
	s.data.Imported = nil
	return n, s.saveLocked()
}
