package cull

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/hashutil"
	"github.com/zack/fuji-tools/internal/immich"
	"github.com/zack/fuji-tools/internal/photo"
)

// immichChecker learns, as full images land in the buffer, whether each shot
// already exists on the Immich server: hash the camera-verbatim file (the
// import pipeline restamps only mtime, so bytes — and checksums — match what
// was uploaded) and bulk-check the sha1 against Immich. The result drives an
// "already uploaded" badge in the timeline and grid. Presence persists
// across runs; a completed import marks its keepers directly.
type immichChecker struct {
	client *immich.Client
	pf     *Prefetcher
	cat    *Catalog
	path   string

	mu      sync.Mutex
	present map[string]byte // shot ID -> '0' absent / '1' present
	dirty   bool

	queue chan *photo.Shot
}

func newImmichChecker(client *immich.Client, pf *Prefetcher, cat *Catalog, cacheDir string) *immichChecker {
	c := &immichChecker{
		client:  client,
		pf:      pf,
		cat:     cat,
		path:    filepath.Join(cacheDir, "immich-present.json"),
		present: map[string]byte{},
		queue:   make(chan *photo.Shot, 8192),
	}
	if raw, err := os.ReadFile(c.path); err == nil {
		m := map[string]string{}
		if json.Unmarshal(raw, &m) == nil {
			for id, v := range m {
				if (v == "0" || v == "1") && cat.Get(id) != nil {
					c.present[id] = v[0]
				}
			}
		}
	}
	if len(c.present) > 0 {
		log.Printf("immich: %d shots' presence known (persisted)", len(c.present))
	}
	go c.worker()
	return c
}

// Enqueue schedules a presence check for a shot whose verbatim file is now
// buffered. Never blocks; already-known shots are skipped.
func (c *immichChecker) Enqueue(s *photo.Shot) {
	c.mu.Lock()
	_, known := c.present[s.ID]
	c.mu.Unlock()
	if known {
		return
	}
	select {
	case c.queue <- s:
	default: // full: the shot re-enqueues next time it lands or at backfill
	}
}

// verbatimFile returns the cached camera-verbatim file to hash for a shot —
// the same bytes an import would upload. RAF-only shots hash the RAF (the
// display JPG is a locally-extracted preview that never goes to Immich).
func (c *immichChecker) verbatimFile(s *photo.Shot) (string, bool) {
	switch {
	case s.Kind == "video":
		return c.pf.CachedFile(s, s.DisplayExt())
	case s.Files["JPG"] != "":
		return c.pf.CachedFile(s, "JPG")
	case s.Files["RAF"] != "":
		return c.pf.CachedFile(s, "RAF")
	}
	return "", false
}

// worker drains the queue in batches: hash locally, one bulk-check per batch.
func (c *immichChecker) worker() {
	flushTick := time.NewTicker(5 * time.Second)
	defer flushTick.Stop()

	var ids, sums []string
	var pending []*photo.Shot
	dispatch := func() {
		if len(ids) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		results, err := c.client.BulkCheck(ctx, ids, sums)
		cancel()
		if err != nil {
			log.Printf("immich: bulk-check: %v (will retry on next landing)", err)
		} else {
			c.mu.Lock()
			marked := 0
			for _, id := range ids {
				r, ok := results[id]
				if ok && r.Action == "reject" && r.Reason == "duplicate" {
					c.present[id] = '1'
					marked++
				} else {
					c.present[id] = '0'
				}
			}
			c.dirty = true
			c.mu.Unlock()
			log.Printf("immich: checked %d shots (%d already uploaded)", len(ids), marked)
		}
		ids, sums, pending = nil, nil, nil
	}

	for {
		select {
		case s := <-c.queue:
			c.mu.Lock()
			_, known := c.present[s.ID]
			c.mu.Unlock()
			if known {
				continue
			}
			path, ok := c.verbatimFile(s)
			if !ok {
				continue
			}
			_, b64, err := hashutil.SHA1File(path)
			if err != nil {
				continue
			}
			ids = append(ids, s.ID)
			sums = append(sums, b64)
			pending = append(pending, s)
			if len(ids) >= 200 {
				dispatch()
			}
		case <-flushTick.C:
			dispatch()
			c.flush()
		}
	}
}

func (c *immichChecker) flush() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	m := make(map[string]string, len(c.present))
	for id, v := range c.present {
		m[id] = string(v)
	}
	c.dirty = false
	c.mu.Unlock()
	raw, _ := json.Marshal(m)
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

// MarkUploaded records shots as present without a server round-trip (called
// after a validated import — the pipeline just proved they're there).
func (c *immichChecker) MarkUploaded(ids []string) {
	c.mu.Lock()
	for _, id := range ids {
		c.present[id] = '1'
	}
	c.dirty = true
	c.mu.Unlock()
}

// Backfill enqueues every shot whose verbatim file is already buffered.
func (c *immichChecker) Backfill() {
	for _, s := range c.cat.Shots {
		if _, ok := c.verbatimFile(s); ok {
			c.Enqueue(s)
		}
	}
}

// States returns one byte per catalog shot: '1' on Immich, '0' not, '-' unknown.
func (c *immichChecker) States() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := make([]byte, len(c.cat.Shots))
	for i, s := range c.cat.Shots {
		if v, ok := c.present[s.ID]; ok {
			buf[i] = v
		} else {
			buf[i] = '-'
		}
	}
	return string(buf)
}
