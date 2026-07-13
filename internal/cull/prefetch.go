package cull

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"os/exec"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/gphoto"
	"github.com/zack/fuji-tools/internal/mtppart"
	"github.com/zack/fuji-tools/internal/photo"
)

// Prefetcher keeps a sliding window of full-size display JPEGs around the
// cursor warm in a local cache directory. The MTP link is single-threaded, so
// a single worker fetches batches (grouped per camera folder to amortize the
// aft-mtp-cli session setup), always starting nearest the cursor.
//
// Photos: JPGs are pulled verbatim; RAF-only shots pull the whole RAF (kept
// for import reuse) and extract its embedded full-res JPEG locally.
// Videos are never prefetched — only pulled on explicit demand.
type Prefetcher struct {
	mu      sync.Mutex
	cond    *sync.Cond
	cat     *Catalog
	backend Backend
	cache   string
	ahead   int
	behind  int
	evict   int
	batch   int // max shots fetched per aft-mtp-cli invocation

	cursor   int
	demand   map[string]bool // shot IDs explicitly requested
	state    map[string]*fetchState
	fetching int
	pause    int // >0 = paused; refcounted (importer and video streaming both pause)
	closed   bool

	imgCancel context.CancelFunc // in-flight image batch; demands preempt it

	stream   *streamState // live camera video stream (owns the MTP claim)
	streamMu sync.Mutex

	// Background thumbnail sweep: runs only when the camera link is otherwise
	// idle, and is killed the moment interactive work arrives.
	thumbFetcher  ThumbFetcher
	thumbDir      string
	thumbs        map[string]byte // shot ID -> thumbMissing/Have/Failed
	thumbCancel   context.CancelFunc
	thumbCursor   int            // sweep origin override (grid viewport hint); -1 = follow cursor
	thumbRetryAt  time.Time      // backoff after transport errors (e.g. camera unplugged)
	thumbStalls   map[string]int // per-shot miss count; skip after 2
	thumbTimeouts int            // consecutive errored batches; drives escalating settle backoff
	thumbRank     map[string]int // shot ID -> 1-based file index within its camera folder
	photoSeq      []photoRank    // photos in catalog order with ranks, for density scans
	partBin       string         // patched aft-mtp-cli with get-part; "" = partial reads off
	noFfmpeg      bool           // ffmpeg missing: posters off, heads unaffected
	partSick      bool           // partial reads returned stale-buffer garbage
	partSickAt    time.Time      // drives the recovery probe
	bulkSick      bool           // bulk reads (get-id) returned stale-buffer garbage
	bulkSickAt    time.Time

	orient      map[string]uint8 // shot ID -> EXIF orientation (absent = unknown)
	orientDirty bool
	healTried   map[string]bool // camera-impossible shots already head-healed (or attempted)
	imageTurn   bool            // last non-demand cycle was a window fill; heads go next

	// Shared persistent partial-read session: heads, orientation sweeps,
	// posters and probes all ride one serve-parts process, paying session
	// setup once instead of per batch (the difference between ~4s per batch
	// and ~0 — vital on phone-class links). Closed whenever one-shot work
	// (bulk pulls, gphoto2, import, streaming) needs the device claim.
	partsMu  sync.Mutex
	partsSrv *mtppart.Server

	onReady func(*photo.Shot) // optional hook: a verbatim file just landed
}

type photoRank struct {
	shot   *photo.Shot
	rank   int
	catIdx int
}

const (
	thumbMissing byte = 0
	thumbHave    byte = 1
	thumbFailed  byte = 2
)

// sickProbeInterval is how often a tripped stale-buffer breaker re-probes.
// Probes are cheap — one file for bulk (the batch cancels on first garbage),
// one 64 KB head for partial reads — and the link is idle while sick, so
// recovery after a power cycle or reconnect lands within seconds of the fix.
const sickProbeInterval = 20 * time.Second

type fetchState struct {
	Status   string // "fetching" | "ready" | "failed"
	Err      string
	Attempts int       // consecutive failures; drives retry backoff
	FailedAt time.Time // when the last failure happened
}

// retryDelay is how long a failed shot waits before the window prefetcher
// tries it again (escalating; user demands via Wait retry immediately).
func retryDelay(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 5 * time.Second
	case attempts == 2:
		return 15 * time.Second
	case attempts == 3:
		return 45 * time.Second
	default:
		return 2 * time.Minute
	}
}

func newPrefetcher(cat *Catalog, backend Backend, cacheDir string, ahead, behind, evictMargin, batch, cursor int) (*Prefetcher, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	if batch < 1 {
		batch = 1
	}
	p := &Prefetcher{
		cat:         cat,
		backend:     backend,
		cache:       cacheDir,
		ahead:       ahead,
		behind:      behind,
		evict:       evictMargin,
		batch:       batch,
		cursor:      cursor,
		demand:      map[string]bool{},
		state:       map[string]*fetchState{},
		thumbs:      map[string]byte{},
		thumbDir:    filepath.Join(cacheDir, "thumbs"),
		thumbCursor: -1,
		thumbStalls: map[string]int{},
		thumbRank:   map[string]int{},
		orient:      map[string]uint8{},
		healTried:   map[string]bool{},
	}
	p.cond = sync.NewCond(&p.mu)

	if tf, ok := backend.(ThumbFetcher); ok {
		if err := gphoto.Ensure(); err != nil {
			log.Printf("thumbnails disabled: %v", err)
		} else {
			p.thumbFetcher = tf
		}
	}
	if bin := mtppart.Bin(); bin != "" {
		p.partBin = bin
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			log.Printf("ffmpeg not found: video posters disabled (head sweep unaffected)")
			p.noFfmpeg = true
		}
	}
	if p.thumbFetcher != nil || p.partBin != "" {
		if err := os.MkdirAll(p.thumbDir, 0o755); err != nil {
			return nil, err
		}
		have := 0
		for _, s := range cat.Shots {
			if s.Kind != "photo" && p.partBin == "" {
				continue
			}
			if st, err := os.Stat(p.ThumbPath(s)); err == nil && st.Size() > 0 {
				p.thumbs[s.ID] = thumbHave
				have++
			}
		}
		log.Printf("thumbs: %d/%d already cached", have, len(cat.Shots))
		p.loadThumbFailed()
	}
	if p.thumbFetcher != nil {
		// gphoto2 selects by 1-based position within the folder; compute each
		// shot's display-file rank among all files in its folder (name order,
		// which matches the camera's object order).
		byDir := map[string][]string{}
		for _, s := range cat.Shots {
			for _, name := range s.Files {
				byDir[s.CameraDir] = append(byDir[s.CameraDir], name)
			}
		}
		rankIn := map[string]map[string]int{}
		for dir, names := range byDir {
			sort.Strings(names)
			m := make(map[string]int, len(names))
			for i, n := range names {
				m[n] = i + 1
			}
			rankIn[dir] = m
		}
		for i, s := range cat.Shots {
			if s.Kind != "photo" {
				continue
			}
			r := rankIn[s.CameraDir][s.Files[s.DisplayExt()]]
			p.thumbRank[s.ID] = r
			if r > 0 {
				p.photoSeq = append(p.photoSeq, photoRank{shot: s, rank: r, catIdx: i})
			}
		}
	}

	// Adopt cache files left by a previous run of the same session. Content
	// is magic-checked: a poisoned camera once delivered stale-buffer garbage
	// with plausible sizes, and size was all the old promotion checked.
	adopted, purged := 0, 0
	for _, s := range cat.Shots {
		path := p.displayPath(s)
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			kind := "jpg"
			if s.Kind == "video" {
				kind = "mov"
			}
			if !mediaValid(path, kind) {
				os.Remove(path)
				purged++
				continue
			}
			p.state[s.ID] = &fetchState{Status: "ready"}
			adopted++
		}
	}
	if adopted > 0 {
		log.Printf("prefetch: adopted %d cached files from previous run", adopted)
	}
	if purged > 0 {
		log.Printf("prefetch: purged %d stale-buffer garbage files banked by a previous run", purged)
	}
	p.loadOrient()
	go p.orientFlusher()
	go p.backfillOrient()
	go p.streamJanitor()
	return p, nil
}

// displayPath is what the UI loads: the JPG for photos, the video file for videos.
func (p *Prefetcher) displayPath(s *photo.Shot) string {
	if s.Kind == "video" {
		return filepath.Join(p.cache, s.SafeID()+"."+strings.ToLower(s.DisplayExt()))
	}
	return filepath.Join(p.cache, s.SafeID()+".jpg")
}

// rafPath holds the full RAF pulled for preview extraction (reused at import).
func (p *Prefetcher) rafPath(s *photo.Shot) string {
	return filepath.Join(p.cache, s.SafeID()+".raf")
}

func (p *Prefetcher) SetCursor(i int) {
	p.mu.Lock()
	p.cursor = i
	p.thumbCursor = -1 // navigation retargets the sweep back to the cursor
	p.interruptThumbsLocked()
	p.mu.Unlock()
	p.cond.Broadcast()
}

// SetThumbHint retargets the thumbnail sweep at the region the grid viewport
// is showing, without moving the culling cursor.
func (p *Prefetcher) SetThumbHint(i int) {
	p.mu.Lock()
	p.thumbCursor = i
	p.mu.Unlock()
	p.cond.Broadcast()
}

// interruptThumbsLocked interrupts an in-flight thumbnail batch (SIGINT to
// gphoto2, which releases the device cleanly; already-received thumbnails are
// banked) so interactive image work gets the camera link promptly.
func (p *Prefetcher) interruptThumbsLocked() {
	if p.thumbCancel != nil {
		p.thumbCancel()
	}
}

// bulkBatch is how many files one image session may carry. Session setup is
// roughly half the cost of a small batch, so batches run large — demands
// preempt an in-flight batch via imgCancel (with per-file incremental
// promotion banking whatever already landed), so a big batch costs latency
// only when its own files are the ones being waited for.
func (p *Prefetcher) bulkBatch() int { return p.batch * 4 }

// interactiveWorkLocked reports whether demands or window prefetch are waiting.
func (p *Prefetcher) interactiveWorkLocked() bool {
	return len(p.demand) > 0 || p.pickLocked() != nil
}

// PauseAndDrain stops new fetches and waits for the in-flight batch to finish
// (used while an import or a camera video stream owns the link). Pauses are
// refcounted so overlapping owners don't resume each other's claim early.
func (p *Prefetcher) PauseAndDrain() {
	p.mu.Lock()
	p.pause++
	p.interruptThumbsLocked()
	for p.fetching > 0 || p.thumbCancel != nil {
		p.cond.Wait()
	}
	p.mu.Unlock()
	p.closePartsServer() // pause owners (import, streaming) take the claim
}

func (p *Prefetcher) Resume() {
	p.mu.Lock()
	if p.pause > 0 {
		p.pause--
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *Prefetcher) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cond.Broadcast()
	p.closePartsServer()
}

// Ensure queues a shot (e.g. a video) for fetching without blocking.
func (p *Prefetcher) Ensure(id string) {
	p.mu.Lock()
	if st, ok := p.state[id]; !ok || st.Status == "failed" {
		delete(p.state, id)
		p.demand[id] = true
	}
	p.interruptThumbsLocked()
	p.interruptImagesLocked(id)
	p.mu.Unlock()
	p.cond.Broadcast()
}

// interruptImagesLocked cancels an in-flight image batch so a blocking
// demand gets the link now — unless the demanded shot is already part of
// that batch (incremental promotion will hand it over the moment it lands).
// Completed files in the cancelled batch are banked; unfinished ones simply
// become eligible again with no failure strike.
func (p *Prefetcher) interruptImagesLocked(id string) {
	if p.imgCancel == nil {
		return
	}
	if st, ok := p.state[id]; ok && st.Status == "fetching" {
		return
	}
	p.imgCancel()
}

// partsReadAt reads via the shared persistent partial-read session, opening
// it on demand. A watchdog closes the session if the camera wedges mid-read
// (Close EOFs the blocked read), so a stale-buffer wedge costs seconds, not
// a hang.
func (p *Prefetcher) partsReadAt(ctx context.Context, objID string, off, size int64) ([]byte, error) {
	p.partsMu.Lock()
	srv := p.partsSrv
	if srv == nil {
		var err error
		srv, err = mtppart.StartServer()
		if err != nil {
			p.partsMu.Unlock()
			return nil, err
		}
		p.partsSrv = srv
	}
	p.partsMu.Unlock()

	type res struct {
		data []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		d, err := srv.ReadAt(objID, off, size)
		ch <- res{d, err}
	}()
	timeout := 20*time.Second + time.Duration(size>>20)*2*time.Second
	select {
	case r := <-ch:
		if r.err != nil {
			p.closePartsServerIf(srv) // process likely dead; reopen next call
		}
		return r.data, r.err
	case <-ctx.Done():
		p.closePartsServerIf(srv) // unblocks the reader goroutine
		<-ch
		return nil, ctx.Err()
	case <-time.After(timeout):
		p.closePartsServerIf(srv)
		r := <-ch
		if r.err == nil {
			return r.data, nil // landed as the watchdog fired
		}
		return nil, fmt.Errorf("partial read timed out — camera wedged?")
	}
}

func (p *Prefetcher) closePartsServerIf(srv *mtppart.Server) {
	p.partsMu.Lock()
	mine := p.partsSrv == srv
	if mine {
		p.partsSrv = nil
	}
	p.partsMu.Unlock()
	if mine {
		srv.Close()
	}
}

// closePartsServer releases the shared partial-read session so one-shot
// invocations (bulk image pulls, gphoto2, import, streaming) can claim the
// device. Reopens lazily on the next partial read.
func (p *Prefetcher) closePartsServer() {
	p.partsMu.Lock()
	srv := p.partsSrv
	p.partsSrv = nil
	p.partsMu.Unlock()
	if srv != nil {
		srv.Close()
	}
}

// VideoHead returns the first 8 MB of a video via the shared partial-read
// session — enough for moov plus the opening frames, which is all poster
// extraction needs. Refuses (rather than queues) while streaming, import or
// a tripped breaker owns the link: the caller treats that as transient.
func (p *Prefetcher) VideoHead(s *photo.Shot, ext string) ([]byte, error) {
	if p.partBin == "" || s == nil || s.ObjectIDs[ext] == "" {
		return nil, fmt.Errorf("video head unavailable")
	}
	p.streamMu.Lock()
	busy := p.stream != nil
	p.streamMu.Unlock()
	p.mu.Lock()
	if p.pause > 0 || p.partSick {
		busy = true
	}
	p.mu.Unlock()
	if busy {
		return nil, fmt.Errorf("camera link busy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := p.partsReadAt(ctx, s.ObjectIDs[ext], 0, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 || string(data[4:8]) != "ftyp" {
		p.mu.Lock()
		p.markPartSickLocked()
		p.mu.Unlock()
		p.closePartsServer()
		return nil, fmt.Errorf("camera returned stale-buffer garbage")
	}
	return data, nil
}

// Nudge wakes the worker and makes tripped breakers and backoffs eligible
// for an immediate probe — the mobile app calls it on foreground resume,
// where "wait out the 20s probe interval" reads as a hang.
func (p *Prefetcher) Nudge() {
	p.mu.Lock()
	p.thumbRetryAt = time.Time{}
	p.thumbTimeouts = 0
	if p.partSick {
		p.partSickAt = time.Now().Add(-sickProbeInterval - time.Second)
	}
	if p.bulkSick {
		p.bulkSickAt = time.Now().Add(-sickProbeInterval - time.Second)
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// Retry clears a failed state so the worker attempts the shot again.
func (p *Prefetcher) Retry(id string) {
	p.mu.Lock()
	if st, ok := p.state[id]; ok && st.Status == "failed" {
		delete(p.state, id)
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// Snapshot returns shot ID -> status for the UI's buffer indicators.
func (p *Prefetcher) Snapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]string, len(p.state))
	for id, st := range p.state {
		out[id] = st.Status
	}
	return out
}

// CachedFile returns the ready cache path for one of the shot's files, used
// to serve videos and to seed imports without touching the camera again.
// Only files whose cached bytes are camera-verbatim are returned.
func (p *Prefetcher) CachedFile(s *photo.Shot, ext string) (string, bool) {
	p.mu.Lock()
	st, ok := p.state[s.ID]
	p.mu.Unlock()
	var path string
	switch {
	case ext == "RAF":
		path = p.rafPath(s) // present when the RAF itself was pulled
	case ext == "JPG" && s.Kind == "photo":
		if _, hasJPG := s.Files["JPG"]; !hasJPG {
			return "", false
		}
		if !ok || st.Status != "ready" {
			return "", false
		}
		path = p.displayPath(s) // verbatim camera JPG
	case photo.ShotKind(ext) == "video":
		if !ok || st.Status != "ready" {
			return "", false
		}
		path = p.displayPath(s)
	default:
		return "", false
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		if want := s.Sizes[ext]; want == 0 || want == fi.Size() {
			return path, true
		}
	}
	return "", false
}

// Wait blocks until the shot's display file is cached (or failed), triggering
// a priority fetch if needed. Returns the cache path.
func (p *Prefetcher) Wait(ctx context.Context, id string) (string, error) {
	s := p.cat.Get(id)
	if s == nil {
		return "", fmt.Errorf("unknown shot %q", id)
	}
	stop := context.AfterFunc(ctx, func() { p.cond.Broadcast() })
	defer stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.state[id]; !ok || st.Status == "failed" {
		delete(p.state, id)
		p.demand[id] = true
	}
	p.interruptThumbsLocked()
	p.interruptImagesLocked(id)
	p.cond.Broadcast()
	for {
		if err := ctx.Err(); err != nil {
			delete(p.demand, id)
			return "", err
		}
		if st, ok := p.state[id]; ok {
			switch st.Status {
			case "ready":
				return p.displayPath(s), nil
			case "failed":
				return "", fmt.Errorf("fetch failed: %s", st.Err)
			}
		}
		p.cond.Wait()
	}
}

// Run is the single fetch worker; call in a goroutine. Interactive image
// fetches always win; thumbnail sweeps fill the idle gaps.
func (p *Prefetcher) Run() {
	// Scheduler heartbeat: backoffs (thumbRetryAt) and breaker probe timers
	// are only evaluated when the worker wakes, and cond.Wait has no timeout
	// — without a periodic broadcast an expired backoff sleeps until the
	// next user interaction (on the phone that read as "camera idle until I
	// scroll, then a burst").
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return
			}
			p.cond.Broadcast()
		}
	}()
	for {
		p.mu.Lock()
		var targets, thumbBatch, orientBatch, healBatch, posterBatch []*photo.Shot
		var probePart bool
		var thumbCtx context.Context
		for {
			if p.closed {
				p.mu.Unlock()
				return
			}
			if p.pause == 0 {
				// Grid-first fairness: window image fills and the head sweep
				// take turns while both have work — a cold buffer would
				// otherwise starve the grid of thumbnails for the entire
				// window fill (minutes of full-size pulls on phone-class
				// links). Demands are user-blocking and always win.
				if p.imageTurn && len(p.demand) == 0 && p.partBin != "" && !p.partSick {
					if healBatch = p.pickHealBatchLocked(orientBatchSize); len(healBatch) > 0 {
						p.imageTurn = false
						thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
						break
					}
				}
				if targets = p.pickLocked(); len(targets) > 0 {
					if len(p.demand) == 0 {
						p.imageTurn = true
					}
					break
				}
				// Tripped partial-read breaker: probe recovery every 3
				// minutes (a power cycle or reconnect cures the camera and
				// streaming/posters/heal should come back on their own).
				if p.partBin != "" && p.partSick && time.Since(p.partSickAt) > sickProbeInterval {
					p.partSickAt = time.Now()
					probePart = true
					break
				}
				if p.thumbFetcher != nil || p.partBin != "" {
					// Idle-work priority: the head sweep first — one 128 KB
					// read per shot yields thumbnail AND orientation together
					// (~150 shots/s), so it precedes the orientation-only
					// sweep (which then only mops up shots that already had
					// thumbs) and leaves the gphoto2 sweep as fallback for
					// shots whose heads carry no embedded thumbnail or when
					// partial reads are unavailable. Posters last.
					if p.partBin != "" && !p.partSick {
						if healBatch = p.pickHealBatchLocked(orientBatchSize); len(healBatch) > 0 {
							thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
							break
						}
						if orientBatch = p.pickOrientBatchLocked(orientBatchSize); len(orientBatch) > 0 {
							thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
							break
						}
					}
					if p.thumbFetcher != nil && (p.partBin == "" || !p.partSick) && len(thumbBatch) == 0 {
						thumbBatch = p.pickThumbsLocked(150)
					}
					if len(thumbBatch) > 0 {
						thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
						break
					}
					if p.partBin != "" && !p.partSick && !p.noFfmpeg {
						if posterBatch = p.pickVideoPosterBatchLocked(posterBatchSize); len(posterBatch) > 0 {
							thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
							break
						}
					}
				}
			}
			p.cond.Wait()
		}

		if len(targets) > 0 {
			for _, s := range targets {
				attempts := 0
				if st, ok := p.state[s.ID]; ok {
					attempts = st.Attempts
				}
				p.state[s.ID] = &fetchState{Status: "fetching", Attempts: attempts}
			}
			p.fetching = len(targets)
			p.mu.Unlock()

			p.closePartsServer() // one-shot pulls need the device claim
			p.fetchBatch(targets)

			p.mu.Lock()
			p.fetching = 0
			p.evictLocked()
			p.mu.Unlock()
			p.cond.Broadcast()
			continue
		}

		p.mu.Unlock()
		switch {
		case probePart:
			p.probePartialReads()
		case len(orientBatch) > 0:
			p.fetchOrientBatch(thumbCtx, orientBatch)
		case len(healBatch) > 0:
			p.fetchHealBatch(thumbCtx, healBatch)
		case len(posterBatch) > 0:
			p.fetchVideoPosterBatch(thumbCtx, posterBatch)
		default:
			p.closePartsServer() // gphoto2 needs the device claim
			p.fetchThumbBatch(thumbCtx, thumbBatch)
		}
		p.mu.Lock()
		p.thumbCancel = nil
		p.mu.Unlock()
		p.cond.Broadcast()
	}
}

// pickThumbsLocked selects up to n photos without thumbnails, nearest the
// sweep origin first (the grid-viewport hint when set, else the cursor),
// all from one camera folder (one cd per invocation).
// thumbWindow is how many folder ranks one gphoto2 span may cover. A span
// fetch costs per rank covered (already-have files are re-fetched and
// discarded), so batches maximize missing-thumbs-per-rank, not proximity.
const thumbWindow = 250

func (p *Prefetcher) pickThumbsLocked(n int) []*photo.Shot {
	if !p.thumbRetryAt.IsZero() && time.Now().Before(p.thumbRetryAt) {
		return nil // backing off after a transport error
	}
	i := p.nearestMissingLocked(p.thumbOriginLocked())
	if i < 0 {
		return nil
	}
	// Serve the origin's region while it is reasonably dense (keeps the
	// grid-viewport hint responsive); once the local field is mostly swept,
	// a nearest-first sweep degrades to one thumbnail per enumeration —
	// jump to the densest remaining window instead.
	local := p.windowBatchLocked(i, n)
	if len(local) >= 40 {
		return local
	}
	if best := p.bestWindowLocked(n); len(best) > 2*len(local) {
		return best
	}
	return local
}

// windowBatchLocked collects up to n missing photos in Shots[i]'s folder
// whose ranks fall within thumbWindow of Shots[i]'s rank.
func (p *Prefetcher) windowBatchLocked(i, n int) []*photo.Shot {
	first := p.cat.Shots[i]
	base := p.thumbRank[first.ID]
	var batch []*photo.Shot
	for j := i; j < len(p.cat.Shots) && len(batch) < n; j++ {
		s := p.cat.Shots[j]
		if s.CameraDir != first.CameraDir {
			break
		}
		r := p.thumbRank[s.ID]
		if r == 0 {
			continue // video
		}
		if r >= base+thumbWindow {
			break
		}
		if s.Kind == "photo" && p.thumbs[s.ID] == thumbMissing {
			batch = append(batch, s)
		}
	}
	return batch
}

// bestWindowLocked finds the densest thumbWindow-wide run of missing thumbs
// across the whole card (two-pointer per folder segment over photos).
func (p *Prefetcher) bestWindowLocked(n int) []*photo.Shot {
	isMissing := func(s *photo.Shot) bool {
		return p.thumbs[s.ID] == thumbMissing
	}
	bestIdx, bestCount := -1, 0
	lo, missing := 0, 0
	seq := p.photoSeq
	for hi := 0; hi < len(seq); hi++ {
		if seq[lo].shot.CameraDir != seq[hi].shot.CameraDir {
			lo, missing = hi, 0
		}
		if isMissing(seq[hi].shot) {
			missing++
		}
		for seq[hi].rank-seq[lo].rank >= thumbWindow {
			if isMissing(seq[lo].shot) {
				missing--
			}
			lo++
		}
		if missing > bestCount {
			bestCount, bestIdx = missing, seq[lo].catIdx
		}
	}
	if bestIdx < 0 {
		return nil
	}
	return p.windowBatchLocked(bestIdx, n)
}

// thumbOriginLocked is where the sweep radiates from: the grid-viewport hint
// when set, else the culling cursor.
func (p *Prefetcher) thumbOriginLocked() int {
	if p.thumbCursor >= 0 && p.thumbCursor < len(p.cat.Shots) {
		return p.thumbCursor
	}
	return p.cursor
}

// nearestMissingLocked returns the catalog index of the shot closest to
// origin that still lacks a thumbnail, or -1 when the sweep is complete.
func (p *Prefetcher) nearestMissingLocked(origin int) int {
	needs := func(s *photo.Shot) bool {
		return s.Kind == "photo" && p.thumbs[s.ID] == thumbMissing
	}
	for d := 0; d < len(p.cat.Shots); d++ {
		if i := origin + d; i < len(p.cat.Shots) && needs(p.cat.Shots[i]) {
			return i
		}
		if i := origin - d; d > 0 && i >= 0 && needs(p.cat.Shots[i]) {
			return i
		}
	}
	return -1
}

// fetchThumbBatch pulls one gphoto2 invocation's worth of thumbnails, selected
// by folder index. gphoto2 pays a per-invocation folder enumeration (~1 s per
// 700 files), so batches are large; interactive work interrupts via the batch
// context (SIGINT — safe, gphoto2 releases the device cleanly and aft-mtp-cli
// retries briefly on residual claim). Results are banked even on error/cancel.
func (p *Prefetcher) fetchThumbBatch(ctx context.Context, batch []*photo.Shot) {
	dir := batch[0].CameraDir
	// One contiguous span (this gphoto2 rejects comma lists; videos inside
	// the span are skipped by gphoto2 without aborting). The picker already
	// bounds batches to a thumbWindow-wide dense region.
	start, end := 0, 0
	for _, s := range batch {
		r := p.thumbRank[s.ID]
		if r <= 0 {
			continue
		}
		if start == 0 || r < start {
			start = r
		}
		if r > end {
			end = r
		}
	}
	if start == 0 {
		return
	}
	workDir, err := os.MkdirTemp(p.thumbDir, "batch-*")
	if err != nil {
		log.Printf("thumbs: mkdtemp: %v", err)
		return
	}
	defer os.RemoveAll(workDir)

	timeout := 60*time.Second + time.Duration(end-start+1)*500*time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, timeout)
	got, runErr := p.thumbFetcher.FetchThumbSpan(cctx, dir, start, end, workDir)
	canceled := ctx.Err() != nil || cctx.Err() != nil
	cancel()

	total := 0
	p.mu.Lock()
	for base, tmp := range got {
		s := p.cat.Get(dir + "/" + base) // self-identified: tolerate ordering drift
		if s == nil || p.thumbs[s.ID] == thumbHave {
			continue
		}
		// An interrupted gphoto2 leaves truncated files behind (nonzero size,
		// no EOI marker) — banking those poisons the cache with thumbnails
		// that can never decode. Validate completeness before accepting.
		if !jpegComplete(tmp) {
			os.Remove(tmp)
			continue
		}
		if os.Rename(tmp, p.ThumbPath(s)) == nil {
			p.thumbs[s.ID] = thumbHave
			total++
		}
	}
	for _, s := range batch {
		if p.thumbs[s.ID] == thumbHave || canceled || runErr != nil {
			continue
		}
		// Clean run but no thumbnail delivered: strike; skip after two so a
		// genuinely thumbless file cannot loop, while one odd run cannot
		// permanently poison a shot.
		p.thumbStalls[s.ID]++
		if p.thumbStalls[s.ID] >= 2 {
			p.thumbs[s.ID] = thumbFailed
			log.Printf("thumbs: no thumbnail for %s/%s after two attempts; skipping", s.CameraDir, s.Files[s.DisplayExt()])
			p.saveThumbFailedLocked()
		}
	}
	if runErr != nil && ctx.Err() == nil {
		p.thumbTimeouts++
		settle := min(48*time.Second, 3*time.Second<<min(4, p.thumbTimeouts-1))
		log.Printf("thumbs: gphoto2 batch in %s: %v (backing off %s)", dir, runErr, settle)
		p.thumbRetryAt = time.Now().Add(settle)
		time.AfterFunc(settle, p.cond.Broadcast)
	} else if runErr == nil {
		p.thumbTimeouts = 0
	}
	p.mu.Unlock()
	if total > 0 {
		log.Printf("thumbs: +%d from %s", total, dir)
	}
}

// jpegComplete reports whether the file both starts with the JPEG SOI marker
// and ends with EOI. Truncated transfers fail the tail check; the Fuji/gphoto2
// fragment bug (a "thumbnail" that is a mid-file slice of image data, ending
// at the true EOI) fails the head check.
func jpegComplete(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < 4 {
		return false
	}
	var head, tail [2]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return false
	}
	if _, err := f.ReadAt(tail[:], st.Size()-2); err != nil {
		return false
	}
	return head[0] == 0xFF && head[1] == 0xD8 && tail[0] == 0xFF && tail[1] == 0xD9
}

// thumbFailedPath persists shots whose thumbnails the camera can never
// deliver (the fragment-blob firmware bug) — without it every restart
// re-downloads ~4 MB of garbage twice per shot before re-striking.
func (p *Prefetcher) thumbFailedPath() string {
	return filepath.Join(p.thumbDir, "camera-impossible.json")
}

func (p *Prefetcher) loadThumbFailed() {
	raw, err := os.ReadFile(p.thumbFailedPath())
	if err != nil {
		return
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return
	}
	n := 0
	for _, id := range ids {
		if s := p.cat.Get(id); s != nil && p.thumbs[id] != thumbHave {
			p.thumbs[id] = thumbFailed
			p.thumbStalls[id] = 2
			n++
		}
	}
	if n > 0 {
		log.Printf("thumbs: %d shots marked camera-impossible (persisted)", n)
	}
}

// saveThumbFailedLocked snapshots the failed set (call with p.mu held).
func (p *Prefetcher) saveThumbFailedLocked() {
	var ids []string
	for id, st := range p.thumbs {
		if st == thumbFailed {
			ids = append(ids, id)
		}
	}
	raw, _ := json.Marshal(ids)
	tmp := p.thumbFailedPath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, p.thumbFailedPath())
	}
}

// posterBatchSize bounds one poster session: 12 heads × 8 MB ≈ 96 MB ≈ 2 s
// of link time on top of the fixed session setup that used to be paid once
// PER VIDEO (~1 poster/s sequential; batched ≈ 4-6/s).
const posterBatchSize = 12

// pickVideoPosterBatchLocked selects the nearest videos lacking posters.
func (p *Prefetcher) pickVideoPosterBatchLocked(n int) []*photo.Shot {
	if !p.thumbRetryAt.IsZero() && time.Now().Before(p.thumbRetryAt) {
		return nil
	}
	needs := func(s *photo.Shot) bool {
		return s.Kind == "video" && p.thumbs[s.ID] == thumbMissing &&
			p.thumbStalls[s.ID] < 2 && s.ObjectIDs[s.DisplayExt()] != ""
	}
	origin := p.thumbOriginLocked()
	var batch []*photo.Shot
	for d := 0; d < len(p.cat.Shots) && len(batch) < n; d++ {
		for _, i := range []int{origin + d, origin - d} {
			if len(batch) >= n || i < 0 || i >= len(p.cat.Shots) || (d == 0 && i != origin) {
				continue
			}
			if s := p.cat.Shots[i]; needs(s) {
				batch = append(batch, s)
			}
		}
	}
	return batch
}

// fetchVideoPosterBatch pulls the heads of a batch of videos in ONE
// partial-read session (Fuji writes moov at the front, so ~8 MB carries the
// index plus the opening frames) and extracts 240px posters via parallel
// ffmpeg. Garbage heads trip the partial-read breaker without striking
// shots — the data was garbage, not the video.
func (p *Prefetcher) fetchVideoPosterBatch(ctx context.Context, batch []*photo.Shot) {
	tmp, err := os.MkdirTemp(p.thumbDir, "vp-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)

	reqs := make([]mtppart.PartReq, len(batch))
	for i, s := range batch {
		reqs[i] = mtppart.PartReq{
			ObjectID: s.ObjectIDs[s.DisplayExt()],
			Offset:   0,
			Size:     8 << 20,
			Dest:     filepath.Join(tmp, s.SafeID()+".mov"),
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second+time.Duration(len(batch))*4*time.Second)
	var runErr error
	for _, r := range reqs {
		if cctx.Err() != nil {
			break
		}
		data, err := p.partsReadAt(cctx, r.ObjectID, r.Offset, r.Size)
		if err != nil {
			runErr = err
			break
		}
		os.WriteFile(r.Dest, data, 0o644)
		if len(data) >= 8 && string(data[4:8]) != "ftyp" {
			break // stale-buffer garbage: stop pulling from a poisoned session
		}
	}
	canceled := ctx.Err() != nil || cctx.Err() != nil
	cancel()
	if runErr != nil && !canceled {
		p.mu.Lock()
		p.thumbRetryAt = time.Now().Add(15 * time.Second)
		p.mu.Unlock()
		log.Printf("video posters: batch: %v", runErr)
	}

	// Extract frames in parallel — ffmpeg is local CPU, the link is done.
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var cnt sync.Mutex
	made, failed, garbage := 0, 0, 0
	for i, s := range batch {
		head := reqs[i].Dest
		if st, err := os.Stat(head); err != nil || st.Size() == 0 {
			continue // not transferred (cancel/error); retry naturally
		}
		if !mediaValid(head, "mov") {
			garbage++
			continue
		}
		wg.Add(1)
		go func(s *photo.Shot, head string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fctx, fcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer fcancel()
			poster := head + ".jpg"
			out, err := exec.CommandContext(fctx, "ffmpeg", "-y", "-loglevel", "error",
				"-i", head, "-frames:v", "1", "-vf", "scale=240:-2", "-q:v", "4", poster).CombinedOutput()
			if err == nil && jpegComplete(poster) && os.Rename(poster, p.ThumbPath(s)) == nil {
				p.mu.Lock()
				p.thumbs[s.ID] = thumbHave
				p.mu.Unlock()
				cnt.Lock()
				made++
				cnt.Unlock()
				return
			}
			log.Printf("video poster: %s: ffmpeg: %v: %.150s", s.ID, err, string(out))
			p.mu.Lock()
			p.thumbStalls[s.ID]++
			if p.thumbStalls[s.ID] >= 2 {
				p.thumbs[s.ID] = thumbFailed
			}
			p.mu.Unlock()
			cnt.Lock()
			failed++
			cnt.Unlock()
		}(s, head)
	}
	wg.Wait()

	if garbage > 0 {
		p.mu.Lock()
		p.markPartSickLocked()
		p.mu.Unlock()
		p.closePartsServer() // poisoned session; probes reopen fresh
		log.Printf("video posters: %d/%d heads returned garbage — pausing partial reads (power-cycle the camera; probing every 20s)", garbage, len(batch))
	}
	if made > 0 || failed > 0 {
		log.Printf("video posters: +%d from camera heads (%d failed)", made, failed)
	}
}

// mediaValid reports whether a file starts like the media it claims to be
// ("jpg", "raf" or "mov"). The X-H2S stale-buffer bug answers reads — bulk
// GetObject included — with replayed MTP responses of plausible LENGTH but
// garbage content, so size checks alone are not proof of a good transfer.
func mediaValid(path, kind string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [16]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	switch kind {
	case "raf":
		return string(b[:8]) == "FUJIFILM"
	case "mov":
		return string(b[4:8]) == "ftyp"
	default:
		return b[0] == 0xFF && b[1] == 0xD8
	}
}

// LinkSick reports tripped camera-transfer circuit breakers for the UIs:
// bulk (get-id returned stale-buffer garbage; clears on the next valid
// transfer) and partial (get-part garbage; sticky until restart).
func (p *Prefetcher) LinkSick() (bulk, partial bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bulkSick, p.partSick
}

// markPartSickLocked trips the partial-read breaker (call with p.mu held).
// A probe retries every sickProbeInterval — the camera recovers on power cycle or
// reconnect, and streaming/posters/heal should come back without a restart.
func (p *Prefetcher) markPartSickLocked() {
	p.partSick = true
	p.partSickAt = time.Now()
}

// probePartialReads tests recovery with one validated 64 KB head.
func (p *Prefetcher) probePartialReads() {
	var target *photo.Shot
	for _, s := range p.cat.Shots {
		if s.Kind == "photo" && s.ObjectIDs[s.DisplayExt()] != "" {
			target = s
			break
		}
	}
	if target == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	head, err := p.partsReadAt(ctx, target.ObjectIDs[target.DisplayExt()], 0, 64<<10)
	cancel()
	if err != nil {
		return // transport error (camera absent?) — next probe shortly
	}
	if len(head) < 2 || head[0] != 0xFF || head[1] != 0xD8 {
		p.closePartsServer() // still poisoned; next probe reopens fresh
		log.Printf("prefetch: partial-read probe still garbage — next probe in 20s (power-cycle the camera)")
		return
	}
	p.mu.Lock()
	p.partSick = false
	p.forgiveThumbFailuresLocked()
	p.mu.Unlock()
	p.cond.Broadcast()
	log.Printf("prefetch: partial reads recovered (probe OK) — streaming, posters and head sweep re-enabled")
}

// forgiveThumbFailuresLocked un-strikes every failed photo thumbnail after
// the camera recovers from a stale-buffer episode. Strikes accumulated while
// sick are false: the gphoto2 fallback harvested garbage blobs, not truth.
// The head sweep re-serves the lot in seconds, so failure is never permanent
// while partial reads exist (call with p.mu held).
func (p *Prefetcher) forgiveThumbFailuresLocked() {
	if p.partBin == "" {
		return
	}
	forgiven := 0
	for _, s := range p.cat.Shots {
		if s.Kind != "photo" {
			continue
		}
		delete(p.healTried, s.ID)
		if p.thumbs[s.ID] == thumbFailed {
			p.thumbs[s.ID] = thumbMissing
			delete(p.thumbStalls, s.ID)
			forgiven++
		}
	}
	if forgiven > 0 {
		p.saveThumbFailedLocked()
		log.Printf("thumbs: forgave %d failed shots for the head sweep to retry", forgiven)
	}
}

// bulkSickLocked reports whether automated pulls should pause because bulk
// reads recently returned stale-buffer garbage. Explicit demands stay allowed
// — they act as recovery probes, and one valid transfer clears the flag (the
// user fixes the camera with a power cycle). Re-probes automatically every
// sickProbeInterval so an idle app also notices recovery.
func (p *Prefetcher) bulkSickLocked() bool {
	return p.bulkSick && time.Since(p.bulkSickAt) < sickProbeInterval
}

// ThumbPath is the cache location of a shot's timeline thumbnail.
func (p *Prefetcher) ThumbPath(s *photo.Shot) string {
	return filepath.Join(p.thumbDir, s.SafeID()+".jpg")
}

// ThumbStates returns one byte per catalog shot: '0' missing, '1' cached,
// '2' unavailable ('-' for shots that never get thumbnails).
func (p *Prefetcher) ThumbStates() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	buf := make([]byte, len(p.cat.Shots))
	have := 0
	for i, s := range p.cat.Shots {
		if (p.thumbFetcher == nil && p.partBin == "") || (s.Kind != "photo" && p.partBin == "") {
			buf[i] = '-'
			continue
		}
		buf[i] = '0' + p.thumbs[s.ID]
		if p.thumbs[s.ID] == thumbHave {
			have++
		}
	}
	return string(buf), have
}

// HasThumb reports whether a cached thumbnail exists for the shot.
func (p *Prefetcher) HasThumb(s *photo.Shot) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.thumbs[s.ID] == thumbHave
}

// setState records a fetch outcome and wakes any Wait()ers. Failures carry an
// attempt count and timestamp so the window prefetcher retries with backoff.
func (p *Prefetcher) setState(id, status, errMsg string) {
	p.mu.Lock()
	attempts := 0
	if st, ok := p.state[id]; ok {
		attempts = st.Attempts
	}
	ns := &fetchState{Status: status, Err: errMsg, Attempts: attempts}
	if status == "failed" {
		ns.Attempts++
		ns.FailedAt = time.Now()
		// Wake the worker when the backoff elapses so retries don't wait
		// for unrelated activity.
		time.AfterFunc(retryDelay(ns.Attempts), p.cond.Broadcast)
	} else if status == "ready" {
		ns.Attempts = 0
	}
	p.state[id] = ns
	delete(p.demand, id)
	p.mu.Unlock()
	p.cond.Broadcast()
}

// pickLocked chooses the next shots to fetch: explicit demands first (alone,
// for latency), else the nearest missing window shot plus same-folder
// neighbors to fill one camera batch.
func (p *Prefetcher) pickLocked() []*photo.Shot {
	needed := func(s *photo.Shot) bool {
		if s == nil {
			return false
		}
		if s.Kind == "video" && !p.demand[s.ID] {
			return false // videos only on demand
		}
		st, has := p.state[s.ID]
		if !has {
			return true
		}
		// Failed shots become eligible again after a backoff — buffering
		// self-heals without the user having to ask for a retry.
		return st.Status == "failed" && time.Since(st.FailedAt) >= retryDelay(st.Attempts)
	}
	// Demands (navigation past the buffer edge, filmstrip jump, video load)
	// win, nearest to cursor first. A demanded photo heads a batch extended
	// with the shots right after it: the user is moving through them, and
	// incremental promotion hands the demanded file over the moment it lands,
	// so the extras stream at link speed instead of paying per-photo session
	// setup. Videos are never extended — their transfer is long already.
	var best *photo.Shot
	bestDist := 1 << 30
	for id := range p.demand {
		s := p.cat.Get(id)
		if !needed(s) {
			continue
		}
		d := p.cat.Index[id] - p.cursor
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = s, d
		}
	}
	if best != nil {
		batch := []*photo.Shot{best}
		if best.Kind == "photo" {
			for i := p.cat.Index[best.ID] + 1; len(batch) < p.bulkBatch() && i < len(p.cat.Shots); i++ {
				s := p.cat.Shots[i]
				if s.CameraDir != best.CameraDir || i > p.cursor+p.ahead {
					break
				}
				if needed(s) {
					batch = append(batch, s)
				}
			}
		}
		return batch
	}
	// Window: current, then ahead (the direction of travel), then behind.
	// Paused while bulk reads are sick — every pull would bank a 10 MB
	// garbage transfer; the periodic re-probe (or any demand) checks recovery.
	if p.bulkSickLocked() {
		return nil
	}
	var first *photo.Shot
	if p.cursor >= 0 && p.cursor < len(p.cat.Shots) && needed(p.cat.Shots[p.cursor]) {
		first = p.cat.Shots[p.cursor]
	}
	for d := 1; first == nil && (d <= p.ahead || d <= p.behind); d++ {
		if d <= p.ahead {
			if i := p.cursor + d; i < len(p.cat.Shots) && needed(p.cat.Shots[i]) {
				first = p.cat.Shots[i]
				break
			}
		}
		if d <= p.behind {
			if i := p.cursor - d; i >= 0 && needed(p.cat.Shots[i]) {
				first = p.cat.Shots[i]
				break
			}
		}
	}
	if first == nil {
		return nil
	}
	// Fill the batch with more missing window shots from the same camera
	// folder (one aft-mtp-cli session covers them all).
	batch := []*photo.Shot{first}
	for d := 1; len(batch) < p.bulkBatch() && d <= p.ahead+p.behind; d++ {
		i := p.cat.Index[first.ID] + d
		if i >= len(p.cat.Shots) || i > p.cursor+p.ahead {
			break
		}
		s := p.cat.Shots[i]
		if s.CameraDir == first.CameraDir && needed(s) {
			batch = append(batch, s)
		}
	}
	return batch
}

// evictLocked drops cached files that drifted far outside the window.
func (p *Prefetcher) evictLocked() {
	for id, st := range p.state {
		if st.Status != "ready" || p.demand[id] {
			continue
		}
		i, ok := p.cat.Index[id]
		if !ok {
			continue
		}
		d := i - p.cursor
		if d < 0 {
			d = -d
		}
		if d > p.evict {
			s := p.cat.Shots[i]
			_ = os.Remove(p.displayPath(s))
			_ = os.Remove(p.rafPath(s))
			delete(p.state, id)
		}
	}
}

// fetchBatch pulls a batch of shots in one backend call. Because file sizes
// are known from discovery, each file is promoted to "ready" the moment its
// bytes are all on disk — a Wait()er on the first file of a batch does not
// sit out the rest of the batch.
func (p *Prefetcher) fetchBatch(targets []*photo.Shot) {
	items := make([]fetchItem, 0, len(targets))
	tmps := make([]string, len(targets))
	expect := make([]int64, len(targets))
	kinds := make([]string, len(targets))
	finished := make([]bool, len(targets))

	for i, s := range targets {
		var srcExt string
		switch {
		case s.Kind == "video":
			srcExt = s.DisplayExt()
		case s.Files["JPG"] != "":
			srcExt = "JPG"
		case s.Files["RAF"] != "":
			srcExt = "RAF"
		default:
			p.setState(s.ID, "failed", "no displayable file in shot")
			finished[i] = true
			continue
		}
		dest := p.displayPath(s)
		if srcExt == "RAF" && s.Kind == "photo" {
			dest = p.rafPath(s)
		}
		kinds[i] = "jpg"
		switch {
		case s.Kind == "video":
			kinds[i] = "mov"
		case srcExt == "RAF":
			kinds[i] = "raf"
		}
		tmps[i] = dest + ".tmp"
		expect[i] = s.Sizes[srcExt]
		items = append(items, fetchItem{
			CameraDir: s.CameraDir, Name: s.Files[srcExt],
			ObjectID: s.ObjectIDs[srcExt], Dest: tmps[i],
		})
	}

	// Bound the transfer: a wedged USB op with no timeout once froze the
	// whole image pipeline for hours (goroutine stuck in [IO wait] on the
	// aft child). On expiry the child gets SIGINT, the batch fails, and the
	// per-shot backoff machinery retries. Demands preempt via imgCancel —
	// large backfill batches must never block an on-screen image.
	fctx, fcancel := context.WithTimeout(context.Background(),
		60*time.Second+time.Duration(len(items))*15*time.Second)
	p.mu.Lock()
	p.imgCancel = fcancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.imgCancel = nil
		p.mu.Unlock()
		fcancel()
	}()
	fetchDone := make(chan error, 1)
	go func() { fetchDone <- p.backend.Fetch(fctx, items) }()

	promote := func(i int) {
		if !mediaValid(tmps[i], kinds[i]) {
			os.Remove(tmps[i])
			log.Printf("prefetch: %s: transfer content is not %s — camera bulk reads are replaying stale buffers; POWER-CYCLE the camera (automated pulls paused, navigation still probes)",
				targets[i].ID, kinds[i])
			p.mu.Lock()
			p.bulkSick, p.bulkSickAt = true, time.Now()
			p.mu.Unlock()
			p.setState(targets[i].ID, "failed", "camera returned stale-buffer garbage — power-cycle the camera")
			finished[i] = true
			fcancel() // the rest of the batch is garbage too; don't pull ~290 MB of it
			return
		}
		if err := p.finalizeShot(targets[i], tmps[i]); err != nil {
			log.Printf("prefetch: %s: %v", targets[i].ID, err)
			p.setState(targets[i].ID, "failed", err.Error())
		} else {
			p.setState(targets[i].ID, "ready", "")
			p.harvestOrient(targets[i])
			if p.onReady != nil {
				p.onReady(targets[i])
			}
			p.mu.Lock()
			if p.bulkSick {
				p.bulkSick = false
				// A valid transfer after garbage means the camera was
				// power-cycled — the only cure — so partial reads (streaming,
				// posters, head-heal) are trustworthy again too. Their own
				// validation re-trips partSick if not.
				if p.partSick {
					p.partSick = false
					p.forgiveThumbFailuresLocked()
					log.Printf("prefetch: camera recovered — re-enabling partial reads")
				} else {
					log.Printf("prefetch: camera bulk reads recovered")
				}
			}
			p.mu.Unlock()
		}
		finished[i] = true
	}

	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case fetchErr := <-fetchDone:
			canceled := fctx.Err() == context.Canceled
			for i, s := range targets {
				if finished[i] {
					continue
				}
				if st, err := os.Stat(tmps[i]); err == nil && st.Size() > 0 &&
					(expect[i] == 0 || st.Size() == expect[i]) {
					promote(i)
					continue
				}
				os.Remove(tmps[i])
				if canceled {
					// Preempted by a demand, not a failure: immediately
					// eligible again, no backoff strike.
					p.mu.Lock()
					delete(p.state, s.ID)
					p.mu.Unlock()
					finished[i] = true
					continue
				}
				msg := "pull produced no data for " + s.ID
				if fetchErr != nil {
					msg = fetchErr.Error()
				}
				log.Printf("prefetch: %s: %s", s.ID, msg)
				p.setState(s.ID, "failed", msg)
				finished[i] = true
			}
			return
		case <-tick.C:
			for i := range targets {
				if finished[i] || expect[i] == 0 {
					continue
				}
				if st, err := os.Stat(tmps[i]); err == nil && st.Size() == expect[i] {
					promote(i)
				}
			}
		}
	}
}

// finalizeShot promotes a completed tmp pull into its cache location,
// extracting the embedded preview for RAF-only shots.
func (p *Prefetcher) finalizeShot(s *photo.Shot, tmp string) error {
	if s.Kind == "photo" && s.Files["JPG"] == "" {
		// RAF-only: keep the RAF, extract its embedded preview locally.
		raf := p.rafPath(s)
		if err := os.Rename(tmp, raf); err != nil {
			return err
		}
		jpgTmp := p.displayPath(s) + ".tmp"
		if err := exif.ExtractPreview(raf, jpgTmp); err != nil {
			os.Remove(jpgTmp)
			return err
		}
		return os.Rename(jpgTmp, p.displayPath(s))
	}
	return os.Rename(tmp, p.displayPath(s))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
