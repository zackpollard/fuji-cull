package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

// store is the canonical decision store for the self-hosted sync server. It is a
// mutex-guarded in-memory model persisted atomically to one JSON file — the
// relay's data is small (decisions for a few cameras) and pushes are debounced,
// so whole-file atomic writes are simpler and safer than an embedded DB while
// giving the same transactional guarantee (the lock IS the transaction).
//
// The generation guard defends against a restored-stale DB: Epoch is minted once
// and re-minted whenever the store is detected to have been rewound; every
// response carries it so a client whose high-water no longer matches re-seeds.
type store struct {
	mu   sync.Mutex
	path string
	data storeData
}

type storeData struct {
	Epoch           string                  `json:"epoch"`
	HighVersionEver int64                   `json:"highVersionEver"`
	Cameras         map[string]*cameraState `json:"cameras"`
}

type cameraState struct {
	Version   int64                   `json:"version"`
	Decisions map[string]*decisionRow `json:"decisions"`
	Resume    map[string]*resumeRow   `json:"resume"`
}

type decisionRow struct {
	D         string       `json:"d"`
	Del       bool         `json:"del,omitempty"`
	HLC       synccore.HLC `json:"hlc"`
	Migrated  bool         `json:"migrated,omitempty"`
	Version   int64        `json:"version"`
	ReceiptTs int64        `json:"receiptTs"`
	Contested bool         `json:"contested,omitempty"`
}

type resumeRow struct {
	Ckey    string       `json:"ckey"`
	HLC     synccore.HLC `json:"hlc"`
	Version int64        `json:"version"`
}

// contestWindowMs: two competing writes whose walls fall within this band, from
// different devices, on a value change, are flagged for the client to surface
// rather than silently resolving by a possibly-skewed clock.
const contestWindowMs = 2 * 3600 * 1000

func openStore(path string) (*store, error) {
	s := &store{path: path, data: storeData{Cameras: map[string]*cameraState{}}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.data.Epoch = newEpoch()
		return s, s.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	if s.data.Cameras == nil {
		s.data.Cameras = map[string]*cameraState{}
	}
	// Generation guard: if the persisted data was rewound below the highest
	// version we ever handed out, the DB was restored stale — mint a fresh epoch
	// so every client re-seeds from its own full local copy.
	var maxVer int64
	for _, c := range s.data.Cameras {
		if c.Version > maxVer {
			maxVer = c.Version
		}
	}
	if s.data.Epoch == "" || maxVer < s.data.HighVersionEver {
		s.data.Epoch = newEpoch()
		s.data.HighVersionEver = maxVer
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *store) save() error {
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

func (s *store) cameraLocked(slug string) *cameraState {
	c := s.data.Cameras[slug]
	if c == nil {
		c = &cameraState{Decisions: map[string]*decisionRow{}, Resume: map[string]*resumeRow{}}
		s.data.Cameras[slug] = c
	}
	return c
}

// push applies a batch under the lock (one transaction), bumping the camera
// version once per accepted row and returning per-item winners.
func (s *store) push(req synccore.PushRequest, nowMs int64) synccore.PushResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	cam := s.cameraLocked(req.Camera)
	resp := synccore.PushResponse{Epoch: s.data.Epoch, ServerNow: nowMs}
	dirty := false

	for _, in := range req.Decisions {
		stored, exists := cam.Decisions[in.Ckey]
		var stHLC synccore.HLC
		var stMig bool
		if exists {
			stHLC, stMig = stored.HLC, stored.Migrated
		}
		if !synccore.Wins(in.HLC, in.Migrated, stHLC, stMig, exists) {
			// reject: return the stored winner so the client converges to it
			resp.Results = append(resp.Results, synccore.AckRow{
				Ckey: in.Ckey, Accepted: false, Version: stored.Version,
				Winner:    synccore.DecisionRow{Ckey: in.Ckey, D: stored.D, Del: stored.Del, HLC: stored.HLC, Migrated: stored.Migrated, Version: stored.Version, Contested: stored.Contested},
				Contested: stored.Contested,
			})
			continue
		}
		contested := exists && !stored.Migrated && !in.Migrated &&
			(stored.D != in.D || stored.Del != in.Del) &&
			stored.HLC.Dev != in.HLC.Dev &&
			absI64(stored.HLC.Wall-in.HLC.Wall) < contestWindowMs
		cam.Version++
		row := &decisionRow{D: in.D, Del: in.Del, HLC: in.HLC, Migrated: in.Migrated, Version: cam.Version, ReceiptTs: nowMs, Contested: contested}
		cam.Decisions[in.Ckey] = row
		if cam.Version > s.data.HighVersionEver {
			s.data.HighVersionEver = cam.Version
		}
		dirty = true
		resp.Results = append(resp.Results, synccore.AckRow{
			Ckey: in.Ckey, Accepted: true, Version: row.Version,
			Winner:    synccore.DecisionRow{Ckey: in.Ckey, D: row.D, Del: row.Del, HLC: row.HLC, Migrated: row.Migrated, Version: row.Version, Contested: row.Contested},
			Contested: contested,
		})
	}

	if req.Resume != nil && req.Resume.Ckey != "" {
		cur := cam.Resume[req.Resume.Dev]
		if cur == nil || cur.HLC.Less(req.Resume.HLC) {
			cam.Version++
			cam.Resume[req.Resume.Dev] = &resumeRow{Ckey: req.Resume.Ckey, HLC: req.Resume.HLC, Version: cam.Version}
			if cam.Version > s.data.HighVersionEver {
				s.data.HighVersionEver = cam.Version
			}
			dirty = true
		}
	}

	resp.CameraVersion = cam.Version
	if dirty {
		_ = s.save()
	}
	return resp
}

// pull returns every row strictly above `since` for a camera, plus the delta
// high-water the client should use as its next `since`.
func (s *store) pull(slug string, since, nowMs int64) synccore.PullResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := synccore.PullResponse{Epoch: s.data.Epoch, ServerNow: nowMs, DeltaHigh: since}
	cam := s.data.Cameras[slug]
	if cam == nil {
		return resp
	}
	for ck, r := range cam.Decisions {
		if r.Version > since {
			resp.Decisions = append(resp.Decisions, synccore.DecisionRow{
				Ckey: ck, D: r.D, Del: r.Del, HLC: r.HLC, Migrated: r.Migrated, Version: r.Version, Contested: r.Contested,
			})
			if r.Version > resp.DeltaHigh {
				resp.DeltaHigh = r.Version
			}
		}
	}
	for dev, r := range cam.Resume {
		if r.Version > since {
			resp.Resume = append(resp.Resume, synccore.ResumeRow{Dev: dev, Ckey: r.Ckey, HLC: r.HLC, Version: r.Version})
			if r.Version > resp.DeltaHigh {
				resp.DeltaHigh = r.Version
			}
		}
	}
	return resp
}

func absI64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func newEpoch() string {
	// a monotonic-ish, collision-resistant generation token
	return time.Now().UTC().Format("20060102T150405") + "-" + randHex(4)
}
