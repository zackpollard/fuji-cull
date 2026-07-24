package cull

import "github.com/zack/fuji-tools/internal/synccore"

// The sync-facing Session API: the durable outbox (unacked records), inbound
// batch merge (ApplyRemote), and ack handling. All merges are HLC-LWW,
// commutative and idempotent, so re-delivery and reconnect order never lose or
// double-apply a decision. Wire types come from synccore so the engine and the
// server merge by the identical rule.

// SetServerRef records the last server clock (ms) for the forward wall clamp.
func (s *Session) SetServerRef(ms int64) {
	s.mu.Lock()
	if ms > s.serverRef {
		s.serverRef = ms
	}
	s.mu.Unlock()
}

// Outbox returns every unacked (SV==0) record as wire rows to push. The session
// file IS the outbox, so this survives app kills for free.
func (s *Session) Outbox() []synccore.DecisionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []synccore.DecisionRow
	for ck, r := range s.data.Records {
		if r.SV == 0 {
			out = append(out, synccore.DecisionRow{Ckey: ck, D: r.D, Del: r.Del, HLC: r.HLC, Migrated: r.Migrated})
		}
	}
	return out
}

// OutboxResume returns this device's resume point if it is unacked, else nil.
func (s *Session) OutboxResume() *synccore.ResumeRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data.Resume[s.data.DeviceID]
	if !ok || c.SV != 0 || c.K == "" {
		return nil
	}
	return &synccore.ResumeRow{Dev: s.data.DeviceID, Ckey: c.K, HLC: c.HLC}
}

// SyncMeta returns the pull high-water, last-seen epoch, and device id.
func (s *Session) SyncMeta() (serverVer int64, epoch, deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ServerVer, s.data.Epoch, s.data.DeviceID
}

// ApplyRemote LWW-merges a batch of pulled rows under one lock and one save.
// deltaHigh advances the pull high-water (pull only, never a push response).
// Returns whether anything changed.
func (s *Session) ApplyRemote(recs []synccore.DecisionRow, cursors []synccore.ResumeRow, epoch string, deltaHigh, serverNow int64) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if serverNow > s.serverRef {
		s.serverRef = serverNow
	}
	for _, in := range recs {
		nr := record{D: in.D, Del: in.Del, HLC: in.HLC, SV: in.Version, Migrated: in.Migrated}
		cur, ok := s.data.Records[in.Ckey]
		if !recordWins(nr, cur, ok) {
			continue
		}
		s.data.Records[in.Ckey] = nr
		s.data.NodeHLC = maxHLC(s.data.NodeHLC, in.HLC)
		s.projectLocked(in.Ckey, nr, "")
		changed = true
	}
	for _, c := range cursors {
		cur, ok := s.data.Resume[c.Dev]
		if !ok || cur.HLC.Less(c.HLC) {
			s.data.Resume[c.Dev] = cursorRec{K: c.Ckey, HLC: c.HLC, SV: c.Version}
			s.data.NodeHLC = maxHLC(s.data.NodeHLC, c.HLC)
			changed = true
		}
	}
	if deltaHigh > s.data.ServerVer {
		s.data.ServerVer = deltaHigh
		changed = true
	}
	if epoch != "" && epoch != s.data.Epoch {
		s.data.Epoch = epoch
		changed = true
	}
	if changed {
		_ = s.saveLocked()
	}
	return changed
}

// AckPush adopts the server's authoritative winners from a push response: it
// LWW-merges each winner (so a losing push still converges toward the server's
// state) and marks a record clean (SV set) only when the record we now hold IS
// the server's winner and we haven't re-edited it since dispatch.
func (s *Session) AckPush(resp synccore.PushResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resp.ServerNow > s.serverRef {
		s.serverRef = resp.ServerNow
	}
	dirty := false
	for _, a := range resp.Results {
		win := record{D: a.Winner.D, Del: a.Winner.Del, HLC: a.Winner.HLC, SV: a.Version, Migrated: a.Winner.Migrated}
		cur, ok := s.data.Records[a.Ckey]
		if recordWins(win, cur, ok) {
			s.data.Records[a.Ckey] = win
			s.data.NodeHLC = maxHLC(s.data.NodeHLC, win.HLC)
			s.projectLocked(a.Ckey, win, "")
			dirty = true
		} else if ok && cur.HLC == a.Winner.HLC && cur.SV == 0 {
			cur.SV = a.Version
			s.data.Records[a.Ckey] = cur
			dirty = true
		}
		// else: re-edited since dispatch -> stays dirty, will re-push. Idempotent.
	}
	if dirty {
		_ = s.saveLocked()
	}
}

// AckResume marks this device's resume point clean once the server accepts it.
func (s *Session) AckResume(version int64, sentHLC hlc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data.Resume[s.data.DeviceID]
	if ok && c.HLC == sentHLC && c.SV == 0 {
		c.SV = version
		s.data.Resume[s.data.DeviceID] = c
		_ = s.saveLocked()
	}
}

// ResetForEpoch handles a server generation change / rewind: forget the pull
// high-water and mark every record dirty so the whole local register re-seeds
// the (possibly restored-stale) server. Idempotent LWW makes this safe.
func (s *Session) ResetForEpoch(newEpoch string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.ServerVer = 0
	s.data.Epoch = newEpoch
	for ck, r := range s.data.Records {
		if r.SV != 0 {
			r.SV = 0
			s.data.Records[ck] = r
		}
	}
	for dev, c := range s.data.Resume {
		if c.SV != 0 {
			c.SV = 0
			s.data.Resume[dev] = c
		}
	}
	_ = s.saveLocked()
}

// CameraSlug returns the persisted identity slug (may be "" before discovery).
func (s *Session) CameraSlug() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Camera
}

// SetCameraSlug persists the identity slug so the syncer can run before/without a
// camera attached.
func (s *Session) SetCameraSlug(slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Camera != slug {
		s.data.Camera = slug
		_ = s.saveLocked()
	}
}
