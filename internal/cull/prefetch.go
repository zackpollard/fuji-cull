package cull

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/gphoto"
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
	paused   bool
	closed   bool

	// Background thumbnail sweep: runs only when the camera link is otherwise
	// idle, and is killed the moment interactive work arrives.
	thumbFetcher ThumbFetcher
	thumbDir     string
	thumbs       map[string]byte // shot ID -> thumbMissing/Have/Failed
	thumbCancel   context.CancelFunc
	thumbCursor   int            // sweep origin override (grid viewport hint); -1 = follow cursor
	thumbRetryAt  time.Time      // backoff after transport errors (e.g. camera unplugged)
	thumbStalls   map[string]int // per-shot miss count; skip after 2
	thumbTimeouts int            // consecutive errored batches; drives escalating settle backoff
	thumbRank     map[string]int // shot ID -> 1-based file index within its camera folder
	photoSeq      []photoRank    // photos in catalog order with ranks, for density scans
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
		cat:      cat,
		backend:  backend,
		cache:    cacheDir,
		ahead:    ahead,
		behind:   behind,
		evict:    evictMargin,
		batch:    batch,
		cursor:   cursor,
		demand:   map[string]bool{},
		state:       map[string]*fetchState{},
		thumbs:      map[string]byte{},
		thumbDir:    filepath.Join(cacheDir, "thumbs"),
		thumbCursor: -1,
		thumbStalls: map[string]int{},
		thumbRank:   map[string]int{},
	}
	p.cond = sync.NewCond(&p.mu)

	if tf, ok := backend.(ThumbFetcher); ok {
		if err := gphoto.Ensure(); err != nil {
			log.Printf("thumbnails disabled: %v", err)
		} else {
			p.thumbFetcher = tf
		}
	}
	if p.thumbFetcher != nil {
		if err := os.MkdirAll(p.thumbDir, 0o755); err != nil {
			return nil, err
		}
		have := 0
		for _, s := range cat.Shots {
			if s.Kind != "photo" {
				continue
			}
			if st, err := os.Stat(p.ThumbPath(s)); err == nil && st.Size() > 0 {
				p.thumbs[s.ID] = thumbHave
				have++
			}
		}
		log.Printf("thumbs: %d/%d already cached", have, len(cat.Shots))

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

	// Adopt cache files left by a previous run of the same session.
	adopted := 0
	for _, s := range cat.Shots {
		if st, err := os.Stat(p.displayPath(s)); err == nil && st.Size() > 0 {
			p.state[s.ID] = &fetchState{Status: "ready"}
			adopted++
		}
	}
	if adopted > 0 {
		log.Printf("prefetch: adopted %d cached files from previous run", adopted)
	}
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

// interactiveWorkLocked reports whether demands or window prefetch are waiting.
func (p *Prefetcher) interactiveWorkLocked() bool {
	return len(p.demand) > 0 || p.pickLocked() != nil
}

// PauseAndDrain stops new fetches and waits for the in-flight batch to finish
// (used while an import owns the camera link).
func (p *Prefetcher) PauseAndDrain() {
	p.mu.Lock()
	p.paused = true
	p.interruptThumbsLocked()
	for p.fetching > 0 || p.thumbCancel != nil {
		p.cond.Wait()
	}
	p.mu.Unlock()
}

func (p *Prefetcher) Resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *Prefetcher) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cond.Broadcast()
}

// Ensure queues a shot (e.g. a video) for fetching without blocking.
func (p *Prefetcher) Ensure(id string) {
	p.mu.Lock()
	if st, ok := p.state[id]; !ok || st.Status == "failed" {
		delete(p.state, id)
		p.demand[id] = true
	}
	p.interruptThumbsLocked()
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
	for {
		p.mu.Lock()
		var targets, thumbBatch []*photo.Shot
		var thumbCtx context.Context
		for {
			if p.closed {
				p.mu.Unlock()
				return
			}
			if !p.paused {
				if targets = p.pickLocked(); len(targets) > 0 {
					break
				}
				if p.thumbFetcher != nil {
					if thumbBatch = p.pickThumbsLocked(150); len(thumbBatch) > 0 {
						thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
						break
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

			p.fetchBatch(targets)

			p.mu.Lock()
			p.fetching = 0
			p.evictLocked()
			p.mu.Unlock()
			p.cond.Broadcast()
			continue
		}

		p.mu.Unlock()
		p.fetchThumbBatch(thumbCtx, thumbBatch)
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
		if p.thumbFetcher == nil || s.Kind != "photo" {
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
			for i := p.cat.Index[best.ID] + 1; len(batch) < p.batch && i < len(p.cat.Shots); i++ {
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
	for d := 1; len(batch) < p.batch && d <= p.ahead; d++ {
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
		tmps[i] = dest + ".tmp"
		expect[i] = s.Sizes[srcExt]
		items = append(items, fetchItem{
			CameraDir: s.CameraDir, Name: s.Files[srcExt],
			ObjectID: s.ObjectIDs[srcExt], Dest: tmps[i],
		})
	}

	fetchDone := make(chan error, 1)
	go func() { fetchDone <- p.backend.Fetch(context.Background(), items) }()

	promote := func(i int) {
		if err := p.finalizeShot(targets[i], tmps[i]); err != nil {
			log.Printf("prefetch: %s: %v", targets[i].ID, err)
			p.setState(targets[i].ID, "failed", err.Error())
		} else {
			p.setState(targets[i].ID, "ready", "")
		}
		finished[i] = true
	}

	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case fetchErr := <-fetchDone:
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
