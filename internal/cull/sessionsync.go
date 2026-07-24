package cull

// The sync-facing Session API: the durable outbox (unacked records), inbound
// batch merge (ApplyRemote), and ack handling. All merges are HLC-LWW,
// commutative and idempotent, so re-delivery and reconnect order never lose or
// double-apply a decision.

// remoteRecord is one decision row as it crosses the wire (push item or pull
// delta). Migrated is set only for a client seeding the server from a v1 file.
type remoteRecord struct {
	Ckey     string `json:"ckey"`
	D        string `json:"d"`
	Del      bool   `json:"del,omitempty"`
	HLC      hlc    `json:"hlc"`
	Migrated bool   `json:"migrated,omitempty"`
	Version  int64  `json:"version,omitempty"` // server-assigned; becomes SV on the client
}

// remoteCursor is one per-device resume point on the wire.
type remoteCursor struct {
	Dev     string `json:"dev"`
	Ckey    string `json:"ckey"`
	HLC     hlc    `json:"hlc"`
	Version int64  `json:"version,omitempty"`
}

// ack is a push-response item: the server's authoritative winner for a key.
type ack struct {
	Ckey    string `json:"ckey"`
	Version int64  `json:"version"`
	Winner  remoteRecord
	Contested bool `json:"contested,omitempty"`
}

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
func (s *Session) Outbox() []remoteRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []remoteRecord
	for ck, r := range s.data.Records {
		if r.SV == 0 {
			out = append(out, remoteRecord{Ckey: ck, D: r.D, Del: r.Del, HLC: r.HLC, Migrated: r.Migrated})
		}
	}
	return out
}

// SyncMeta returns the pull high-water and last-seen epoch for building a pull.
func (s *Session) SyncMeta() (serverVer int64, epoch, deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ServerVer, s.data.Epoch, s.data.DeviceID
}

// ApplyRemote LWW-merges a batch of pulled rows under one lock and one save.
// Advancing ServerVer is the caller's business via deltaHigh (pull only, never a
// push response). Returns whether anything changed.
func (s *Session) ApplyRemote(recs []remoteRecord, cursors []remoteCursor, epoch string, deltaHigh, serverNow int64) (changed bool) {
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
		if !ok || cur.HLC.less(c.HLC) {
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
func (s *Session) AckPush(acks []ack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := false
	for _, a := range acks {
		win := record{D: a.Winner.D, Del: a.Winner.Del, HLC: a.Winner.HLC, SV: a.Version, Migrated: a.Winner.Migrated}
		cur, ok := s.data.Records[a.Ckey]
		if recordWins(win, cur, ok) {
			// server's winner beats what we hold: adopt it, and it's clean
			s.data.Records[a.Ckey] = win
			s.data.NodeHLC = maxHLC(s.data.NodeHLC, win.HLC)
			s.projectLocked(a.Ckey, win, "")
			dirty = true
		} else if ok && cur.HLC == a.Winner.HLC && cur.SV == 0 {
			// what we hold IS the server's winner and unchanged since dispatch: clean it
			cur.SV = a.Version
			s.data.Records[a.Ckey] = cur
			dirty = true
		}
		// else: we've re-edited since dispatch (newer HLC, SV still 0) -> stays
		// dirty and will re-push. Idempotent.
	}
	if dirty {
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
