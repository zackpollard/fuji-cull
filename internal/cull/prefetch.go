package cull

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"os/exec"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/gphoto"
	"github.com/zack/fuji-tools/internal/jpegmeta"
	"github.com/zack/fuji-tools/internal/mtpcli"
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
	navGen   uint64          // bumped per SetCursor; debounces the stream release
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
	camera        cameraReader   // iOS: partial reads ride the camera Transport instead of aft
	noFfmpeg      bool           // ffmpeg missing: posters off, heads unaffected
	localThumbs   bool           // dir backend: thumbnails come from source files (sim path)
	partSick      bool           // partial reads returned stale-buffer garbage
	partSickAt    time.Time      // drives the recovery probe
	// emptyBatches counts consecutive batches where every pull came back with
	// zero bytes — the "camera stopped answering" wedge, distinct from the
	// stale-buffer one.
	emptyBatches int
	bulkSick     bool // bulk reads (get-id) returned stale-buffer garbage
	bulkSickAt   time.Time

	orient      map[string]uint8 // shot ID -> EXIF orientation (absent = unknown)
	orientDirty bool
	sharp       map[string]float64 // shot ID -> focus score (absent = unmeasured)
	sharpDirty  bool
	taken       map[string]int64 // shot ID -> EXIF capture time (unix); groups bursts
	takenDirty  bool
	healTried   map[string]bool // camera-impossible shots already head-healed (or attempted)
	imageTurn   bool            // last non-demand cycle was a window fill; heads go next

	// Shared persistent partial-read session: heads, orientation sweeps,
	// posters and probes all ride one serve-parts process, paying session
	// setup once instead of per batch (the difference between ~4s per batch
	// and ~0 — vital on phone-class links). Closed whenever one-shot work
	// (bulk pulls, gphoto2, import, streaming) needs the device claim.
	partsMu  sync.Mutex
	partsSrv *mtppart.Server

	// Converting a pulled file is ffmpeg work, not camera work. Doing it in
	// the fetch loop meant a HEIF-heavy card transferred in bursts and then
	// sat idle: ~0.4s of ffmpeg per shot against ~0.1s of transfer, so the
	// link spent most of a batch waiting on a CPU that had cores to spare.
	// Conversions run on their own bounded pool instead, and the loop goes
	// back to pulling the moment the bytes are down.
	convSem    chan struct{}
	converting int // in-flight conversions, so Close can drain them

	onReady func(*photo.Shot) // optional hook: a verbatim file just landed
	// onStaleHandles fires when the camera is proven to have rebound its
	// object handles, so the catalog cache can be dropped and re-read.
	onStaleHandles func()
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
		convSem:     make(chan struct{}, convWorkers()),
		cursor:      cursor,
		demand:      map[string]bool{},
		state:       map[string]*fetchState{},
		thumbs:      map[string]byte{},
		thumbDir:    filepath.Join(cacheDir, "thumbs"),
		thumbCursor: -1,
		thumbStalls: map[string]int{},
		thumbRank:   map[string]int{},
		orient:      map[string]uint8{},
		sharp:       map[string]float64{},
		taken:       map[string]int64{},
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
		if _, err := exec.LookPath(ffmpegBin()); err != nil {
			log.Printf("ffmpeg not found: video posters disabled (head sweep unaffected)")
			p.noFfmpeg = true
		}
	}
	if ib, ok := backend.(*iccBackend); ok {
		// iOS: partial reads ride the PTP transport, so the head sweep,
		// orientation and chunked pulls all work with no subprocess. Posters
		// are the host's job here (no exec to run ffmpeg) — the app pulls
		// each clip's head via /api/videohead and decodes frame 0 itself,
		// exactly as the Android build does. Note that leaves videos '0' in
		// ThumbStates forever, so a client counting only engine thumbs
		// undercounts by the video count.
		p.camera = ib
		p.noFfmpeg = true
	}
	if _, ok := backend.(*dirBackend); ok {
		p.localThumbs = true // every file is directly readable; thumbnail from source
		if _, err := exec.LookPath(ffmpegBin()); err != nil {
			// no exec on mobile: posters are the host's job there too, and
			// reporting them "available" would leave every video thumb-less
			p.noFfmpeg = true
		}
	}
	if p.thumbFetcher != nil || p.partsOK() || p.localThumbs {
		if err := os.MkdirAll(p.thumbDir, 0o755); err != nil {
			return nil, err
		}
		have := 0
		for _, s := range cat.Shots {
			if s.Kind != "photo" && !p.partsOK() {
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
			// Size the file against what the camera says it should be. The old
			// check only asked whether the file was non-empty, so a truncated
			// leftover was adopted as ready and never re-pulled — the viewer
			// showed a broken photo for a shot the engine called good.
			// RAF-only shots are exempt: their display file is a preview we
			// extracted, not a camera-verbatim copy.
			if want := verbatimSize(s); want > 0 && st.Size() != want {
				os.Remove(path)
				purged++
				continue
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
	// Load every persisted store BEFORE starting anything that touches them:
	// backfillOrient harvests capture times into p.taken, so launching it
	// ahead of loadTaken raced a locked writer against an unlocked one and
	// killed the process with "concurrent map writes" on startup.
	p.loadOrient()
	p.loadSharp()
	p.loadTaken()
	go p.orientFlusher()
	go p.backfillOrient()
	go p.sharpFlusher()
	go p.sharpSweep()
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

// originalPath holds a camera file kept verbatim that cannot be shown as it
// stands — a RAF, or a HEIF — pulled once so a displayable JPEG can be derived
// from it, and reused at import. The extension is preserved so a cache written
// by an older build (which only ever stored ".raf") still resolves.
func (p *Prefetcher) originalPath(s *photo.Shot, ext string) string {
	return filepath.Join(p.cache, s.SafeID()+"."+strings.ToLower(ext))
}

// needsConvert reports whether a shot's display file has to be derived rather
// than pulled: a RAF yields its embedded preview, a HEIF a transcode.
func needsConvert(ext string) bool { return ext == "RAF" || photo.IsHEIF(ext) }

// AltExt names the rendition a shot carries besides the one on display — the
// raw, when the camera wrote a JPEG or HEIF alongside it — or "" for the lone
// files that make up most of a card. The viewer shows the camera's own
// rendering (see photo.DisplayExt), so without being told, a raw+JPEG pair is
// indistinguishable from a lone JPEG, and you cannot see what a keep will
// actually carry into the import.
func AltExt(s *photo.Shot) string {
	if s.Kind != "photo" {
		return ""
	}
	if _, ok := s.Files["RAF"]; ok && s.DisplayExt() != "RAF" {
		return "RAF"
	}
	return ""
}

// streamCloseDebounce delays the post-navigation stream release so that rapid
// tabbing through a bank of videos supersedes it: only a cursor that has
// SETTLED releases the camera. Firing on every hop tore down the stream mpv was
// mid-load on, which left the video the user landed on stuck unloaded (its
// mpv load errored during the churn and the GUI won't re-trigger it).
const streamCloseDebounce = 400 * time.Millisecond

func (p *Prefetcher) SetCursor(i int) {
	p.mu.Lock()
	p.cursor = i
	p.thumbCursor = -1 // navigation retargets the sweep back to the cursor
	p.interruptThumbsLocked()
	p.navGen++
	gen := p.navGen
	p.mu.Unlock()
	// Once navigation settles (see streamCloseDebounce), release a video stream
	// the cursor has left so photo prefetch resumes ~0.4s later instead of
	// waiting out the janitor's 20s idle — without thrashing the stream while
	// you skip through consecutive videos.
	time.AfterFunc(streamCloseDebounce, func() {
		p.mu.Lock()
		superseded := p.navGen != gen
		p.mu.Unlock()
		if !superseded {
			p.closeStreamIfElsewhere()
		}
	})
	p.cond.Broadcast()
}

// thumbHintJump is how far the grid viewport must move (in catalog index)
// before a settling hint aborts the in-flight sweep batch. Ordinary scrolling
// nudges the hint tens of shots and keeps the batch; a scrub across the card is
// thousands and should refocus immediately.
const thumbHintJump = 150

// SetThumbHint retargets the thumbnail sweep at the region the grid viewport
// is showing, without moving the culling cursor.
func (p *Prefetcher) SetThumbHint(i int) {
	p.mu.Lock()
	prev := p.thumbCursor
	if prev < 0 {
		prev = p.cursor
	}
	p.thumbCursor = i
	// A fast scrub lands the viewport far from where the in-flight batch was
	// picked; finishing ~150 now-offscreen heads before refocusing is exactly
	// the lag you see waiting for thumbnails at the bottom. Abort it on a big
	// jump so the loop re-picks nearest the new viewport (partial results are
	// already banked; the shared parts session survives the cancel).
	if d := i - prev; p.thumbCancel != nil && (d > thumbHintJump || d < -thumbHintJump) {
		p.interruptThumbsLocked()
	}
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

// nearWindow is how many shots ahead of the cursor count as "about to be
// looked at". Sized so a brisk pan cannot cross it before a fetch batch
// completes.
const nearWindow = 40

// nearWindowThinLocked reports whether shots just ahead of the cursor have
// never been fetched. Taking turns with the head sweep is right for a cold
// card — the grid would otherwise stay blank for the whole window fill — but
// not while the viewer is walking straight into a hole. There, every other
// batch spent on heads is a stall the user meets head-on, so images take every
// turn until the ground just ahead is covered.
func (p *Prefetcher) nearWindowThinLocked() bool {
	near := min(p.ahead, nearWindow)
	for d := 1; d <= near; d++ {
		i := p.cursor + d
		if i >= len(p.cat.Shots) {
			break
		}
		s := p.cat.Shots[i]
		if s.Kind == "video" {
			continue // videos are fetched on demand, never by the window
		}
		// Only never-attempted shots are holes: one in flight is already
		// being dealt with, and a failed one is waiting out its backoff.
		if _, ok := p.state[s.ID]; !ok {
			return true
		}
	}
	return false
}

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
	// Interrupt in-flight work (parts-based pulls stop cleanly BETWEEN
	// chunks) and drain the worker before touching the sessions — closing
	// the serve-parts process under an active transfer escalates to a hard
	// kill, and a hard-killed MTP session wedges the camera (URB timeouts
	// on the next connect). Seen in the field via settings/rescan restarts.
	p.mu.Lock()
	p.closed = true
	p.interruptThumbsLocked()
	if p.imgCancel != nil {
		p.imgCancel()
	}
	p.mu.Unlock()
	p.cond.Broadcast()

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.cond.Broadcast() // keep the drain loop re-checking its deadline
			}
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	p.mu.Lock()
	for (p.fetching > 0 || p.converting > 0 || p.thumbCancel != nil) && time.Now().Before(deadline) {
		p.cond.Wait()
	}
	p.mu.Unlock()
	close(done)

	// both persistent sessions close gracefully NOW — waiting for the
	// stream janitor risks the process dying first
	p.CloseStream()
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

// cameraReader reads byte ranges straight off the camera without a subprocess
// (the iOS camera Transport). Implemented by iccBackend.
type cameraReader interface {
	readAt(objectID string, offset, size int64) ([]byte, error)
}

// partsOK reports whether partial reads are available at all — either the
// patched aft binary (desktop/Android) or an injected Transport (iOS). It gates
// the head sweep, orientation sweep, posters and chunked pulls.
func (p *Prefetcher) partsOK() bool { return p.partBin != "" || p.camera != nil }

// partsReadAt reads via the shared persistent partial-read session, opening
// it on demand. A watchdog closes the session if the camera wedges mid-read
// (Close EOFs the blocked read), so a stale-buffer wedge costs seconds, not
// a hang.
func (p *Prefetcher) partsReadAt(ctx context.Context, objID string, off, size int64) ([]byte, error) {
	// iOS: ImageCaptureCore owns the session, so there is no process to spawn,
	// claim or close — just call through. Cancellation is honored by returning
	// early; the transport enforces its own timeout (kept above the engine's)
	// so a wedged camera cannot block us forever.
	if p.camera != nil {
		type res struct {
			data []byte
			err  error
		}
		ch := make(chan res, 1)
		go func() {
			d, err := p.camera.readAt(objID, off, size)
			ch <- res{d, err}
		}()
		timeout := 30*time.Second + time.Duration(size>>20)*2*time.Second
		select {
		case r := <-ch:
			return r.data, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(timeout):
			return nil, fmt.Errorf("camera partial read timed out — camera wedged?")
		}
	}

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
		// A read that returns nothing without saying why is a dead session, not
		// an empty file: the caller cannot tell it from a legitimate end-of-
		// object, so it wrote a zero-byte file, reported success, and the
		// prefetcher retried the same shot forever against a session that could
		// never answer. Treat it as the failure it is and recycle the session
		// so the next attempt starts a fresh one.
		if r.err == nil && len(r.data) == 0 && size > 0 {
			p.closePartsServerIf(srv)
			if ctx.Err() == nil {
				mtpcli.NoteTransportResult(true)
			}
			return nil, fmt.Errorf("partial read returned no data for %s at offset %d — session was dead", objID, off)
		}
		if r.err != nil {
			p.closePartsServerIf(srv) // process likely dead; reopen next call
			// feed the link-dead detector: a stale fd (post-reset, wedged
			// camera) fails HERE on Android, not in one-shot invocations —
			// without this the Kotlin connection rebuild never triggers
			if ctx.Err() == nil {
				mtpcli.NoteTransportResult(true)
			}
		} else {
			mtpcli.NoteTransportResult(false)
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
	if s == nil {
		return nil, fmt.Errorf("video head unavailable")
	}
	// directly-readable video (dir backend, or an already-pulled cache copy):
	// serve the head off the file — no camera, no link gating
	if path, ok := p.backend.LocalPath(s, ext); ok {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, 8<<20)
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, err
		}
		return buf[:n], nil
	}
	if !p.partsOK() || s.ObjectIDs[ext] == "" {
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

// Ready reports whether a shot's display file is already in the buffer. Unlike
// Snapshot this is a single map lookup, so a caller asking about one shot at a
// time — the desktop decode pool deciding what it can decode without blocking
// — does not pay for a copy of the whole table on every question.
func (p *Prefetcher) Ready(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[id]
	return ok && st.Status == "ready"
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
	case needsConvert(ext):
		path = p.originalPath(s, ext) // present when the original itself was pulled
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
	// Already buffered: hand the path over and touch nothing else. Asking for
	// a file that is on disk must not disturb the camera, and saying otherwise
	// is ruinous — interruptImagesLocked cancels whatever batch is in flight,
	// so callers that ask about buffered shots in bulk (the desktop decode
	// pool does it per frame, remote clients per image) would cancel the
	// current transfer over and over. No batch then ever completes, the buffer
	// drains, and interrupting transfers mid-flight is what tips this camera
	// into replaying stale buffers.
	if st, ok := p.state[id]; ok && st.Status == "ready" {
		return p.displayPath(s), nil
	}
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
			// broadcast BEFORE exiting on close: Close()'s drain loop
			// relies on these ticks to re-check its deadline
			p.cond.Broadcast()
			if closed {
				return
			}
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
				if p.imageTurn && len(p.demand) == 0 && p.partsOK() && !p.partSick && !p.nearWindowThinLocked() {
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
				if p.partsOK() && p.partSick && time.Since(p.partSickAt) > sickProbeInterval {
					p.partSickAt = time.Now()
					probePart = true
					break
				}
				if p.thumbFetcher != nil || p.partsOK() {
					// Idle-work priority: the head sweep first — one 128 KB
					// read per shot yields thumbnail AND orientation together
					// (~150 shots/s), so it precedes the orientation-only
					// sweep (which then only mops up shots that already had
					// thumbs) and leaves the gphoto2 sweep as fallback for
					// shots whose heads carry no embedded thumbnail or when
					// partial reads are unavailable. Posters last.
					if p.partsOK() && !p.partSick {
						if healBatch = p.pickHealBatchLocked(orientBatchSize); len(healBatch) > 0 {
							thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
							break
						}
						if orientBatch = p.pickOrientBatchLocked(orientBatchSize); len(orientBatch) > 0 {
							thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
							break
						}
					}
					if p.thumbFetcher != nil && (!p.partsOK() || !p.partSick) && len(thumbBatch) == 0 {
						thumbBatch = p.pickThumbsLocked(150)
					}
					if len(thumbBatch) > 0 {
						thumbCtx, p.thumbCancel = context.WithCancel(context.Background())
						break
					}
					if p.partsOK() && !p.partSick && !p.noFfmpeg {
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

			if !p.partsOK() {
				p.closePartsServer() // one-shot pulls need the device claim
			}
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
			args := []string{"-y", "-loglevel", "error", "-i", head, "-frames:v", "1"}
			// the bundled minimal ffmpeg's C-only swscale RESIZE path emits
			// garbage (decode itself is clean — verified); extract full-res
			// there and let the UI downsample. System ffmpeg scales fine.
			if os.Getenv("FUJI_FFMPEG") == "" {
				args = append(args, "-vf", "scale=240:-2")
			}
			args = append(args, "-q:v", "4", poster)
			out, err := exec.CommandContext(fctx, ffmpegBin(), args...).CombinedOutput()
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

// ffmpegBin resolves ffmpeg, honoring an env override for platforms without
// a PATH-installed copy (Android bundles a minimal build as a jniLib —
// crucially, ffmpeg's software HEVC decoder handles the 4:2:2 10-bit
// footage that no Android system codec can touch).
func ffmpegBin() string {
	if p := os.Getenv("FUJI_FFMPEG"); p != "" {
		return p
	}
	return "ffmpeg"
}

// PostersAvailable reports whether engine-side poster extraction runs here.
func (p *Prefetcher) PostersAvailable() bool {
	return p.partsOK() && !p.noFfmpeg
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
	case "heif", "mov":
		// both are ISO-BMFF: the box size leads, then the "ftyp" box type
		return string(b[4:8]) == "ftyp"
	default:
		if b[0] != 0xFF || b[1] != 0xD8 {
			return false
		}
		// A truncated transfer still starts like a JPEG, so the header alone
		// proves nothing: it passed every check and was cached as good, and
		// the viewer then failed to decode a photo the engine reported as
		// ready. Require the end-of-image marker too.
		return jpegComplete(path)
	}
}

// verbatimSize is the size the display file should have when it is a
// camera-verbatim copy, or 0 when it is something we generated locally.
func verbatimSize(s *photo.Shot) int64 {
	switch {
	case s.Kind == "video":
		return s.Sizes[s.DisplayExt()]
	case s.Files["JPG"] != "":
		return s.Sizes["JPG"]
	}
	return 0 // RAF-only: displayPath holds an extracted preview
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
	if !p.partsOK() {
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
		if (p.thumbFetcher == nil && !p.partsOK() && !p.localThumbs) || (s.Kind != "photo" && !p.partsOK()) {
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
			if e := s.DisplayExt(); needsConvert(e) {
				_ = os.Remove(p.originalPath(s, e))
			}
			p.removePreviews(s)
			delete(p.state, id)
		}
	}
}

// partialReadable reports whether every file in a batch can be read to its end
// through the partial-read session. GetPartialObject's offset is 32-bit and the
// X-H2S has no GetPartialObject64 (see streamPartialLimit, verified in the
// field), so a longer file fails at the same byte however often it is retried.
// Streaming already caps itself at that ceiling and offers a full local pull
// instead; this is the other half of that — the pull it offers has to actually
// take the one-shot path, which streams from the start and never names an
// offset. A single oversized file disqualifies the batch, since they share one
// session and the one-shot path handles the whole set.
func partialReadable(sizes []int64) bool {
	for _, sz := range sizes {
		if sz > streamPartialLimit {
			return false
		}
	}
	return true
}

// fetchItemsViaParts pulls full files through the persistent partial-read
// session in 8 MB chunks, writing progressively so incremental promotion
// still hands each file over the moment its bytes land. Cancellation stops
// between chunks with the camera session intact.
func (p *Prefetcher) fetchItemsViaParts(ctx context.Context, items []fetchItem, sizes []int64) error {
	return p.fetchItemsViaPartsProgress(ctx, items, sizes, nil)
}

// fetchItemsViaPartsProgress is fetchItemsViaParts with a per-chunk callback.
// A caller showing progress needs it: a single video can be several gigabytes,
// and crediting bytes only when a file completes leaves the display frozen for
// minutes while the link is working perfectly. onBytes is called with the size
// of each chunk as it lands, never under a lock.
func (p *Prefetcher) fetchItemsViaPartsProgress(ctx context.Context, items []fetchItem, sizes []int64, onBytes func(int64)) error {
	// 8 MiB a read, and measured rather than assumed: on a 4.23 GB clip a
	// single read costs the same at 98% into the file as at the start
	// (120-216ms throughout, no trend), and sustained throughput is 62.0 MB/s
	// over the first 200 MB against 62.8 MB/s over 200 MB at the 90% mark. So
	// the camera does not re-seek per request and reading deep into a large
	// file is not penalised. A one-shot whole-object pull of the same file
	// manages 70 MB/s — about 13% better, which does not pay for reinstating
	// the per-chunk aft invocations that tip this camera into replaying stale
	// buffers. Chunked stays; the one-shot path is for files past the 4 GiB
	// offset ceiling only.
	const chunk = 8 << 20
	for n, it := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if it.ObjectID == "" {
			return fmt.Errorf("no MTP object ID for %s/%s", it.CameraDir, it.Name)
		}
		if sizes[n] > streamPartialLimit {
			return fmt.Errorf("%s/%s is %d bytes: past the offset GetPartialObject can address — needs a whole-object pull",
				it.CameraDir, it.Name, sizes[n])
		}
		out, err := os.Create(it.Dest)
		if err != nil {
			return err
		}
		var off int64
		for {
			if ctx.Err() != nil {
				out.Close()
				return ctx.Err()
			}
			want := int64(chunk)
			if sizes[n] > 0 && off+want > sizes[n] {
				want = sizes[n] - off
			}
			if want <= 0 {
				break
			}
			data, err := p.partsReadAt(ctx, it.ObjectID, off, want)
			if err != nil {
				out.Close()
				return err
			}
			// Only an empty read means end-of-object. A short-but-non-empty
			// read is normal mid-transfer — MTP is free to return less than
			// asked — and treating it as EOF truncated every file a fraction
			// short of its size, which then failed validation and was retried
			// forever against a camera that was answering perfectly well.
			if len(data) == 0 {
				break
			}
			if _, werr := out.Write(data); werr != nil {
				out.Close()
				return werr
			}
			off += int64(len(data))
			if onBytes != nil {
				onBytes(int64(len(data)))
			}
		}
		out.Close()
		// Guard the caller's contract. A short read is only end-of-object when
		// we have all the bytes we were promised; otherwise the transfer was
		// cut off, and returning nil hands the caller a truncated file with no
		// error to act on — which it then retries forever, since nothing
		// distinguishes "this shot is empty" from "the session died mid-file".
		switch {
		case off == 0:
			return fmt.Errorf("%s/%s: partial-read session produced no data", it.CameraDir, it.Name)
		case sizes[n] > 0 && off < sizes[n]:
			return fmt.Errorf("%s/%s: partial-read session stopped at %d of %d bytes",
				it.CameraDir, it.Name, off, sizes[n])
		}
	}
	return nil
}

// fetchBatch pulls a batch of shots in one backend call. Because file sizes
// are known from discovery, each file is promoted to "ready" the moment its
// bytes are all on disk — a Wait()er on the first file of a batch does not
// sit out the rest of the batch.
func (p *Prefetcher) fetchBatch(targets []*photo.Shot) {
	items := make([]fetchItem, 0, len(targets))
	sizes := make([]int64, 0, len(targets))
	tmps := make([]string, len(targets))
	expect := make([]int64, len(targets))
	kinds := make([]string, len(targets))
	finished := make([]bool, len(targets))

	for i, s := range targets {
		srcExt := s.DisplayExt()
		if srcExt == "" {
			p.setState(s.ID, "failed", "no displayable file in shot")
			finished[i] = true
			continue
		}
		dest := p.displayPath(s)
		if s.Kind == "photo" && needsConvert(srcExt) {
			dest = p.originalPath(s, srcExt)
		}
		kinds[i] = "jpg"
		switch {
		case s.Kind == "video":
			kinds[i] = "mov"
		case photo.IsHEIF(srcExt):
			kinds[i] = "heif"
		case srcExt == "RAF":
			kinds[i] = "raf"
		}
		tmps[i] = dest + ".tmp"
		expect[i] = s.Sizes[srcExt]
		items = append(items, fetchItem{
			CameraDir: s.CameraDir, Name: s.Files[srcExt],
			ObjectID: s.ObjectIDs[srcExt], Dest: tmps[i],
		})
		sizes = append(sizes, expect[i])
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
	// Wherever the patched binary exists (Android's usbfs fd AND the desktop's
	// persistent serve-parts session), full pulls ride the partial-read
	// session: demand preemption then stops cleanly BETWEEN chunks. One-shot
	// aft processes get SIGKILLed mid-URB when preempted faster than they can
	// exit — the kill that wedges the camera off the bus while swiping through
	// full-screen photos, tripping it into stale-buffer mode.
	// A file past the partial-read ceiling has to be pulled whole, or it fails
	// at the same offset every time no matter how often it is retried — which
	// is what made a >4 GiB clip unpullable even when asked for by name.
	useParts := p.partsOK() && partialReadable(sizes)
	fetchDone := make(chan error, 1)
	go func() {
		if !useParts {
			fetchDone <- p.backend.Fetch(fctx, items)
			return
		}
		err := p.fetchItemsViaParts(fctx, items, sizes)
		// The partial-read session on this camera can stop delivering a
		// fraction short of the end of a file and then return nothing for the
		// tail, while a one-shot pull of the same object returns it complete
		// and byte-exact. Rather than fail the batch — which retried forever
		// against a camera that was answering perfectly well — drop the
		// session and finish the job the way we know works.
		if err != nil && fctx.Err() == nil {
			log.Printf("prefetch: partial-read session came up short (%v) — retrying batch with one-shot pulls", err)
			p.closePartsServer()
			err = p.backend.Fetch(fctx, items)
		}
		fetchDone <- err
	}()

	promote := func(i int) {
		p.mu.Lock()
		p.emptyBatches = 0 // the camera answered; the streak is broken
		p.mu.Unlock()
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
		// Right size, right media type — but is it the right FILE? With a live
		// session the camera can be asked outright for one round trip, so
		// every promoted file is checked by name; the EXIF timestamp is the
		// fallback where no session exists (one-shot pulls, iOS).
		if name, ok := p.handleNameViaSession(targets[i].ObjectIDs[srcExtOf(targets[i])]); ok {
			if want := targets[i].Files[srcExtOf(targets[i])]; want != "" && name != want {
				log.Printf("prefetch: %s: handle names %q, not %q — discarding and re-reading the catalog",
					targets[i].ID, name, want)
				os.Remove(tmps[i])
				p.setState(targets[i].ID, "failed", "handle pointed at a different file — catalog re-read scheduled")
				finished[i] = true
				if p.onStaleHandles != nil {
					p.onStaleHandles()
				}
				return
			}
		} else if p.identitySuspect(targets[i], tmps[i]) {
			if p.checkHandleRebound(targets[i], srcExtOf(targets[i])) {
				log.Printf("prefetch: %s: downloaded bytes are a different photo than the catalog expected — discarding", targets[i].ID)
				os.Remove(tmps[i])
				p.setState(targets[i].ID, "failed", "handle pointed at a different file — catalog re-read scheduled")
				finished[i] = true
				return
			}
		}
		shot, tmp := targets[i], tmps[i]
		finished[i] = true // the batch is done with it either way
		// A shot that has to be transcoded goes to the converter pool: the
		// file is down, and holding the fetch loop for ffmpeg is what left
		// the camera idle. The shot stays "fetching" until the conversion
		// lands, so nothing re-picks it and a waiter still blocks correctly.
		if shot.Kind == "photo" && needsConvert(shot.DisplayExt()) {
			p.mu.Lock()
			p.converting++
			p.mu.Unlock()
			go func() {
				p.convSem <- struct{}{}
				defer func() {
					<-p.convSem
					p.mu.Lock()
					p.converting--
					p.mu.Unlock()
					p.cond.Broadcast() // Close's drain re-checks on this
				}()
				p.completeShot(shot, tmp)
			}()
			return
		}
		p.completeShot(shot, tmp)
	}

	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case fetchErr := <-fetchDone:
			canceled := fctx.Err() == context.Canceled
			// identical failures collapse to one log line per batch — a
			// disconnected camera otherwise floods the ring with the same
			// error × batch size × retries, burying what came before
			failLogged := 0
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
				if st, serr := os.Stat(tmps[i]); serr == nil && st.Size() > 0 {
					msg = fmt.Sprintf("incomplete pull for %s: %d of %d bytes", s.ID, st.Size(), expect[i])
					// A pull that lands short is the only cheap hint that the
					// handle we asked for is no longer the file we think it
					// is. Ask the camera outright: if the handle now names a
					// different file, every cached binding is suspect, and
					// carrying on would keep serving the WRONG photo under
					// the right name — the size check is the only thing
					// standing between that and a wrong upload.
					if p.checkHandleRebound(s, srcExtOf(s)) {
						msg += " — camera has rebound its object handles; re-reading the catalog"
					}
				}
				if fetchErr != nil {
					msg = fetchErr.Error()
				}
				if failLogged == 0 {
					log.Printf("prefetch: %s: %s", s.ID, msg)
				}
				failLogged++
				p.setState(s.ID, "failed", msg)
				finished[i] = true
			}
			if failLogged > 1 {
				log.Printf("prefetch: … and %d more shots in this batch failed the same way", failLogged-1)
			}
			// A camera handing back nothing at all is as wedged as one handing
			// back stale buffers, but only the latter tripped the breaker — so
			// this failure retried forever, hammering a camera that could not
			// answer and leaving the UI with no idea anything was wrong. Two
			// consecutive all-empty batches is the signal: one can be a
			// preemption or a single bad file.
			if fetchErr == nil && failLogged == len(targets) && failLogged > 0 {
				p.mu.Lock()
				p.emptyBatches++
				n := p.emptyBatches
				if n >= 2 && !p.bulkSick {
					p.bulkSick, p.bulkSickAt = true, time.Now()
					log.Printf("prefetch: %d consecutive batches returned no data — the camera has stopped answering; UNPLUG THE USB CABLE and reconnect (turning the camera off does not re-enumerate it). Automated pulls paused.", n)
				}
				p.mu.Unlock()
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

// convWorkers is how many conversions may run at once. Sized well below the
// core count: ffmpeg is itself threaded and the viewer needs cores to decode
// with. Enough that conversion stays ahead of the camera (~4-5 files/s) rather
// than setting the pace, which is all that is being asked of it.
func convWorkers() int {
	n := runtime.NumCPU() / 4
	if n < 2 {
		n = 2
	}
	if n > 6 {
		n = 6
	}
	return n
}

// completeShot turns a downloaded temp file into the shot the viewer can use
// and publishes the result. For a HEIF or a RAF that means a transcode, which
// is why callers may run this off the fetch loop.
func (p *Prefetcher) completeShot(s *photo.Shot, tmp string) {
	if err := p.finalizeShot(s, tmp); err != nil {
		log.Printf("prefetch: %s: %v", s.ID, err)
		p.setState(s.ID, "failed", err.Error())
		return
	}
	p.setState(s.ID, "ready", "")
	p.harvestOrient(s)
	if p.onReady != nil {
		p.onReady(s)
	}
	p.mu.Lock()
	if p.bulkSick {
		p.bulkSick = false
		// A valid transfer after garbage means the camera was power-cycled —
		// the only cure — so partial reads (streaming, posters, head-heal)
		// are trustworthy again too. Their own validation re-trips partSick
		// if not.
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

// finalizeShot promotes a completed tmp pull into its cache location,
// extracting the embedded preview for RAF-only shots.
func (p *Prefetcher) finalizeShot(s *photo.Shot, tmp string) error {
	if src := s.DisplayExt(); s.Kind == "photo" && needsConvert(src) {
		// Keep the original — it is what an import uploads — and derive the
		// JPEG the viewer shows: a RAF carries an embedded preview, a HEIF has
		// to be transcoded.
		orig := p.originalPath(s, src)
		if err := os.Rename(tmp, orig); err != nil {
			return err
		}
		jpgTmp := p.displayPath(s) + ".tmp"
		var err error
		if photo.IsHEIF(src) {
			err = heifToJPEG(orig, jpgTmp)
		} else {
			err = exif.ExtractPreview(orig, jpgTmp)
		}
		if err != nil {
			os.Remove(jpgTmp)
			return err
		}
		return os.Rename(jpgTmp, p.displayPath(s))
	}
	return os.Rename(tmp, p.displayPath(s))
}

// heifTranscodeTimeout bounds one conversion. A 6240x4160 HEIF takes ~0.4s on
// a desktop; the ceiling is only there so a wedged ffmpeg cannot stall the
// buffer window behind it.
const heifTranscodeTimeout = 60 * time.Second

// heifToJPEG renders a HEIF still to a JPEG the viewer can decode. HEIF is
// HEVC in an ISO-BMFF container, so neither libjpeg-turbo nor the Go image
// package can read one; ffmpeg is already a dependency for video posters and
// decodes it in one pass, applying any rotation the container asks for (which
// is why the result needs no EXIF orientation of its own).
func heifToJPEG(src, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), heifTranscodeTimeout)
	defer cancel()
	// -f mjpeg is not optional: the destination is a ".jpg.tmp" staging path,
	// and ffmpeg picks its muxer from the extension, so it refuses ".tmp"
	// outright ("Unable to choose an output format").
	out, err := exec.CommandContext(ctx, ffmpegBin(), "-v", "error", "-y",
		"-i", src, "-frames:v", "1", "-q:v", "3", "-f", "mjpeg", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("heif transcode %s: %w: %.200s", filepath.Base(src), err, out)
	}
	st, serr := os.Stat(dst)
	if serr != nil || st.Size() == 0 {
		return fmt.Errorf("heif transcode %s produced no image", filepath.Base(src))
	}
	return nil
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

// identitySuspect reports whether a downloaded file's own EXIF capture time
// disagrees with the timestamp the camera gave for that object.
//
// Size is not identity. A handle rebound to a different file of the same byte
// count passes the size check, and mediaValid only confirms the bytes are a
// JPEG — not WHICH JPEG. The file's embedded capture time is the cheapest
// thing that actually distinguishes them, and it is already read for
// orientation, so this costs nothing extra.
//
// Returns false whenever either side is unknown (videos carry no EXIF
// DateTimeOriginal, and older cached listings have no timestamp): the point is
// to catch a definite disagreement, never to reject on absence.
func (p *Prefetcher) identitySuspect(s *photo.Shot, path string) bool {
	if s.Kind != "photo" || s.Taken == "" {
		return false
	}
	want := captureUnix(normalizePTPTime(s.Taken))
	if want == 0 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if n < 4 {
		return false
	}
	got := captureUnix(jpegmeta.DateTimeOriginal(head[:n]))
	if got == 0 {
		return false
	}
	// A second of slack: the camera's listing and the EXIF field are written
	// by the same clock but not necessarily the same instant.
	if d := got - want; d > 1 || d < -1 {
		return true
	}
	return false
}

// normalizePTPTime turns "20260714T151530" into the EXIF-style layout that
// captureUnix parses.
func normalizePTPTime(raw string) string {
	if len(raw) < 15 || raw[8] != 'T' {
		return raw
	}
	return raw[0:4] + ":" + raw[4:6] + ":" + raw[6:8] + " " +
		raw[9:11] + ":" + raw[11:13] + ":" + raw[13:15]
}

// srcExtOf is the extension fetchBatch would have pulled for a shot.
func srcExtOf(s *photo.Shot) string { return s.DisplayExt() }

// handleNames asks the open partial-read session what a handle is called.
// Falls back to nothing when there is no session — the caller then relies on
// the slower confirmation path.
func (p *Prefetcher) handleNameViaSession(objID string) (string, bool) {
	p.mu.Lock()
	srv := p.partsSrv
	p.mu.Unlock()
	if srv == nil {
		return "", false
	}
	name, err := srv.NameOf(objID)
	if err != nil || name == "" {
		return "", false
	}
	return name, true
}

// checkHandleRebound asks the camera what a handle actually points at and
// reports whether it has been rebound to a different file. On a mismatch it
// drops the catalog cache so the next start re-reads the card, because every
// other cached binding is equally suspect.
func (p *Prefetcher) checkHandleRebound(s *photo.Shot, ext string) bool {
	objID := s.ObjectIDs[ext]
	want := s.Files[ext]
	if objID == "" || want == "" {
		return false
	}
	got, ok := p.handleNameViaSession(objID)
	if !ok {
		// No session: fall back to a one-shot lookup (slow, but this only
		// runs when something already looks wrong).
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		entries, err := mtpcli.InfoByIDs(ctx, []string{objID})
		if err != nil || len(entries) == 0 {
			return false
		}
		got = entries[0].Name
	}
	if got == want {
		return false
	}
	log.Printf("prefetch: handle %s is %q on the camera but %q in the catalog — the card's object handles have been rebound; dropping the catalog cache (restart, or Settings -> full rescan, to re-read it)",
		objID, got, want)
	if p.onStaleHandles != nil {
		p.onStaleHandles()
	}
	return true
}
