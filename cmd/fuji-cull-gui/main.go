// fuji-cull-gui: native frontend for fuji-cull. Runs the same engine as the
// web UI in-process (the HTTP server stays up for remote/iPad use) and adds
// what the browser can't: libjpeg-turbo decode across all cores and GPU
// textures the app owns outright.
//
// Keys mirror the web UI: arrows navigate, K keep, X reject, C clear,
// U undo, G next undecided, Z 1:1 zoom, wheel zoom, drag pan, Q/Esc quit.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/veandco/go-sdl2/sdl"
	"github.com/veandco/go-sdl2/ttf"

	"github.com/zack/fuji-tools/internal/cull"
	"github.com/zack/fuji-tools/internal/mpvsw"
	"github.com/zack/fuji-tools/internal/photo"
	"github.com/zack/fuji-tools/internal/turbo"
)

/* ── palette (mirrors the web UI) ─────────────────────────── */
var (
	colBG       = sdl.Color{R: 0x0b, G: 0x0c, B: 0x0b, A: 255}
	colPanel    = sdl.Color{R: 0x12, G: 0x14, B: 0x12, A: 255}
	colFG       = sdl.Color{R: 0xe8, G: 0xe6, B: 0xdf, A: 255}
	colDim      = sdl.Color{R: 0x7d, G: 0x81, B: 0x7b, A: 255}
	colKeep     = sdl.Color{R: 0x37, G: 0xd6, B: 0x7a, A: 255}
	colReject   = sdl.Color{R: 0xff, G: 0x5a, B: 0x3c, A: 255}
	colAmber    = sdl.Color{R: 0xff, G: 0xb4, B: 0x2e, A: 255}
	colBuffered = sdl.Color{R: 0x2f, G: 0x7f, B: 0xe0, A: 255}
	colDecoded  = sdl.Color{R: 0x2d, G: 0xe0, B: 0xc8, A: 255}
	colTickBG   = sdl.Color{R: 0x1d, G: 0x20, B: 0x1d, A: 255}
)

/* ── decode pool: libjpeg-turbo across cores ──────────────── */

type decoded struct {
	img  *turbo.Image
	err  error
	when time.Time
}

type decodePool struct {
	mu       sync.Mutex
	app      *cull.App
	want     []string // priority order, first = most urgent
	inflight map[string]bool
	done     map[string]*decoded
}

func newDecodePool(app *cull.App, workers int) *decodePool {
	p := &decodePool{app: app, inflight: map[string]bool{}, done: map[string]*decoded{}}
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

// SetWant replaces the priority list (called each frame with the cursor
// window). Entries already decoded or inflight are skipped by workers.
func (p *decodePool) SetWant(ids []string) {
	p.mu.Lock()
	p.want = ids
	p.mu.Unlock()
}

func (p *decodePool) Get(id string) *decoded {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.done[id]
	if d != nil && d.err != nil && time.Since(d.when) > 3*time.Second {
		delete(p.done, id) // auto-retry: forget the failure
		return nil
	}
	return d
}

// Forget drops decoded frames not in keep (bounded RAM).
func (p *decodePool) Prune(keep map[string]bool) {
	p.mu.Lock()
	for id := range p.done {
		if !keep[id] {
			delete(p.done, id)
		}
	}
	p.mu.Unlock()
}

func (p *decodePool) next() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.want {
		if p.inflight[id] || p.done[id] != nil {
			continue
		}
		p.inflight[id] = true
		return id, true
	}
	return "", false
}

func (p *decodePool) worker() {
	for {
		id, ok := p.next()
		if !ok {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		path, err := p.app.WaitImage(ctx, id) // camera fetch if not buffered
		cancel()
		var d decoded
		d.when = time.Now()
		if err != nil {
			d.err = err
		} else {
			d.img, d.err = turbo.DecodeFile(path)
		}
		p.mu.Lock()
		delete(p.inflight, id)
		p.done[id] = &d
		p.mu.Unlock()
	}
}

/* ── texture caches ───────────────────────────────────────── */

type texEntry struct {
	tex    *sdl.Texture
	w, h   int32
	used   time.Time
	orient int // EXIF orientation baked into the pixels (thumbs only)
}

type texCache struct {
	m       map[string]*texEntry
	cap     int
	protect string // never evict this id (the on-screen texture)
}

func newTexCache(cap int) *texCache { return &texCache{m: map[string]*texEntry{}, cap: cap} }

func (c *texCache) get(id string) *texEntry {
	e := c.m[id]
	if e != nil {
		e.used = time.Now()
	}
	return e
}

func (c *texCache) drop(id string) {
	if e := c.m[id]; e != nil {
		e.tex.Destroy()
		delete(c.m, id)
	}
}

func (c *texCache) put(id string, e *texEntry) {
	e.used = time.Now()
	c.m[id] = e
	for len(c.m) > c.cap {
		oldest, ot := "", time.Now()
		for k, v := range c.m {
			if k == c.protect {
				continue
			}
			if v.used.Before(ot) {
				oldest, ot = k, v.used
			}
		}
		if oldest == "" {
			return
		}
		c.m[oldest].tex.Destroy()
		delete(c.m, oldest)
	}
}

func uploadRGBA(r *sdl.Renderer, img *turbo.Image) (*texEntry, error) {
	tex, err := r.CreateTexture(uint32(sdl.PIXELFORMAT_ABGR8888), sdl.TEXTUREACCESS_STATIC, int32(img.W), int32(img.H))
	if err != nil {
		return nil, err
	}
	if err := tex.Update(nil, unsafe.Pointer(&img.Pix[0]), img.W*4); err != nil {
		tex.Destroy()
		return nil, err
	}
	tex.SetBlendMode(sdl.BLENDMODE_NONE)
	return &texEntry{tex: tex, w: int32(img.W), h: int32(img.H)}, nil
}

/* ── UI state ─────────────────────────────────────────────── */

type ui struct {
	app    *cull.App
	pool   *decodePool
	ren    *sdl.Renderer
	win    *sdl.Window
	font   *ttf.Font
	fontSm *ttf.Font

	shots     []*photo.Shot
	decisions map[string]string
	cursor    int
	undo      []struct {
		idx  int
		prev string
	}

	full   *texCache // full-res textures
	thumbs *texCache // strip thumbnails
	texts  *texCache // rendered strings

	// viewer transform (CSS-pixel semantics like the web UI)
	scale, tx, ty float64
	fit           float64
	natW, natH    int32
	curTexID      string
	lastTex       *texEntry
	zoomMem       *zoomMem

	panning    bool
	panStart   [2]int32
	panStartTx [2]float64

	fetchStates map[string]string // server-side disk-buffer states (blue stripe)
	orients     string            // per-shot EXIF orientation chars ('1'-'8', '0' unknown)
	camBulkSick bool              // camera transfer breakers (stale-buffer bug)
	camPartSick bool
	frameN      int
	lastWinW    int32
	lastWinH    int32

	mode int // modeViewer | modeGrid | modeImport

	mpv        *mpvsw.Player
	videoID    string
	videoTex   *sdl.Texture
	videoTexW  int32
	videoTexH  int32
	videoBar   sdl.Rect

	gridTop        int
	lastHint       int
	lastGridCursor int

	impDest  string
	impAlbum string
	impField int
	impError string

	decodeAhead  int
	decodeBehind int

	thumbBad map[string]time.Time // corrupt thumb files, negative-cached
}

const (
	modeViewer = iota
	modeGrid
	modeImport
)

type zoomMem struct{ scale, cx, cy, aspect float64 }

func main() {
	var o cull.Options
	flag.StringVar(&o.Listen, "listen", "127.0.0.1:8787", "HTTP listen address for the built-in web UI")
	flag.StringVar(&o.BackendName, "backend", "cli", "camera access: cli or dir")
	flag.StringVar(&o.Root, "root", "", "dir backend root")
	flag.StringVar(&o.CameraRoot, "camera-root", "/SLOT 1/DCIM,/SLOT 2/DCIM", "camera DCIM paths")
	flag.StringVar(&o.SessionName, "session", "default", "session name")
	flag.StringVar(&o.CacheDir, "cache-dir", "", "image buffer directory")
	flag.IntVar(&o.Ahead, "ahead", 150, "shots to buffer ahead")
	flag.IntVar(&o.Behind, "behind", 50, "shots to buffer behind")
	flag.IntVar(&o.EvictMargin, "evict-margin", 600, "disk eviction distance")
	flag.IntVar(&o.Batch, "batch", 6, "files per camera invocation")
	flag.StringVar(&o.Dest, "dest", "", "import destination")
	flag.StringVar(&o.ImmichURL, "immich-url", os.Getenv("IMMICH_URL"), "Immich URL")
	flag.StringVar(&o.ImmichKey, "immich-key", os.Getenv("IMMICH_API_KEY"), "Immich API key")
	flag.StringVar(&o.ImmichAlbum, "immich-album", "", "Immich album")
	flag.BoolVar(&o.SkipImmich, "skip-immich", false, "skip Immich")
	flag.IntVar(&o.Retries, "retries", 3, "immich retries")
	flag.IntVar(&o.UploadConc, "upload-concurrency", 4, "parallel uploads")
	flag.IntVar(&o.HashConc, "hash-concurrency", 4, "parallel hashing")
	decodeAhead := flag.Int("decode-ahead", 28, "decoded frames to hold ahead of the cursor (~104 MB RAM each)")
	decodeBehind := flag.Int("decode-behind", 8, "decoded frames to hold behind the cursor")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	app, handler, err := cull.Start(o)
	if err != nil {
		log.Fatalf("%v", err)
	}
	go func() {
		if err := http.ListenAndServe(o.Listen, handler); err != nil {
			log.Printf("http: %v", err)
		}
	}()
	log.Printf("web UI also available at http://%s", o.Listen)

	if err := run(app, *decodeAhead, *decodeBehind); err != nil {
		log.Fatalf("%v", err)
	}
}

func findMonoFont() string {
	out, err := exec.Command("fc-match", "--format=%{file}", "monospace").Output()
	if err == nil && len(out) > 0 {
		return strings.TrimSpace(string(out))
	}
	return "/usr/share/fonts/TTF/DejaVuSansMono.ttf"
}

func run(app *cull.App, decodeAhead, decodeBehind int) error {
	runtime.LockOSThread()
	if err := sdl.Init(sdl.INIT_VIDEO | sdl.INIT_EVENTS); err != nil {
		return err
	}
	defer sdl.Quit()
	if err := ttf.Init(); err != nil {
		return err
	}
	win, err := sdl.CreateWindow("fuji-cull", sdl.WINDOWPOS_CENTERED, sdl.WINDOWPOS_CENTERED,
		1600, 1000, sdl.WINDOW_RESIZABLE|sdl.WINDOW_ALLOW_HIGHDPI)
	if err != nil {
		return err
	}
	defer win.Destroy()
	ren, err := sdl.CreateRenderer(win, -1, sdl.RENDERER_ACCELERATED|sdl.RENDERER_PRESENTVSYNC)
	if err != nil {
		return err
	}
	defer ren.Destroy()

	fontPath := findMonoFont()
	font, err := ttf.OpenFont(fontPath, 15)
	if err != nil {
		return fmt.Errorf("font %s: %w", fontPath, err)
	}
	fontSm, _ := ttf.OpenFont(fontPath, 12)

	workers := runtime.NumCPU() - 2
	if workers < 2 {
		workers = 2
	}
	u := &ui{
		app: app, ren: ren, win: win, font: font, fontSm: fontSm,
		pool:        newDecodePool(app, workers),
		decodeAhead: decodeAhead, decodeBehind: decodeBehind,
		full:      newTexCache(16),
		thumbs:    newTexCache(400),
		texts:     newTexCache(256),
		decisions: map[string]string{},
	}
	log.Printf("gui: %d turbo decode workers", workers)

	for u.frame() {
	}
	return nil
}

/* ── per-frame ────────────────────────────────────────────── */

func (u *ui) frame() bool {
	for ev := sdl.PollEvent(); ev != nil; ev = sdl.PollEvent() {
		if !u.handleEvent(ev) {
			return false
		}
	}

	if u.shots == nil && u.app.Ready() {
		u.shots = u.app.Shots()
		u.decisions = u.app.Decisions()
		u.cursor = u.app.Cursor()
		if u.cursor < 0 || u.cursor >= len(u.shots) {
			u.cursor = 0
		}
		u.impDest, u.impAlbum = u.app.Defaults()
	}

	// window resize: refit the viewer transform (zoomMem preserved)
	if w, h := u.win.GetSize(); w != u.lastWinW || h != u.lastWinH {
		u.lastWinW, u.lastWinH = w, h
		u.curTexID = ""
	}

	u.ren.SetDrawColor(colBG.R, colBG.G, colBG.B, 255)
	u.ren.Clear()

	if u.shots != nil && u.cursor < len(u.shots) {
		u.full.protect = u.shots[u.cursor].ID
	}
	if u.shots == nil {
		stage, files, errMsg := u.app.Discovery()
		msg := "READING CAMERA INDEX"
		sub := fmt.Sprintf("%s · %d files", stage, files)
		if errMsg != "" {
			msg, sub = "DISCOVERY FAILED", errMsg
		}
		w, h := u.win.GetSize()
		u.text(u.font, msg, colDim, w/2, h/2-14, true)
		u.text(u.fontSm, sub, colDim, w/2, h/2+12, true)
	} else {
		u.frameN++
		if u.fetchStates == nil || u.frameN%30 == 0 {
			u.fetchStates = u.app.FetchStates()
			u.orients = u.app.Orientations()
			u.camBulkSick, u.camPartSick = u.app.CameraSick()
		}
		u.updateWants()
		// A 104 MB photo texture upload mid-playback stalls the render
		// thread and stutters the video: photos wait until playback stops.
		videoPlaying := u.mode == modeViewer && u.cursor < len(u.shots) &&
			u.shots[u.cursor].Kind == "video" && u.videoID != ""
		if !videoPlaying {
			u.uploadReady()
		}
		switch u.mode {
		case modeGrid:
			u.drawGrid()
			u.drawHeader()
		case modeImport:
			u.drawViewer()
			u.drawHeader()
			u.drawStrip()
			u.drawImportPanel()
		default:
			u.drawViewer()
			u.drawHeader()
			u.drawStrip()
		}
	}

	u.ren.Present()
	return true
}

// updateWants hands the decode pool the priority window around the cursor.
func (u *ui) updateWants() {
	var ids []string
	add := func(i int) {
		if i >= 0 && i < len(u.shots) && u.shots[i].Kind == "photo" {
			ids = append(ids, u.shots[i].ID)
		}
	}
	add(u.cursor)
	// Forward-biased: scrubbing is directional, so most of the runway goes
	// ahead of the cursor.
	max := u.decodeAhead
	if u.decodeBehind > max {
		max = u.decodeBehind
	}
	for d := 1; d <= max; d++ {
		if d <= u.decodeAhead {
			add(u.cursor + d)
		}
		if d <= u.decodeBehind {
			add(u.cursor - d)
		}
	}
	u.pool.SetWant(ids)
	keep := map[string]bool{}
	for _, id := range ids {
		keep[id] = true
	}
	u.pool.Prune(keep)
}

// uploadReady moves at most one decoded frame into a GPU texture per frame.
func (u *ui) uploadReady() {
	check := func(i int) bool {
		if i < 0 || i >= len(u.shots) || u.shots[i].Kind != "photo" {
			return false
		}
		id := u.shots[i].ID
		if u.full.get(id) != nil {
			return false
		}
		d := u.pool.Get(id)
		if d == nil || d.err != nil || d.img == nil {
			return false
		}
		if te, err := uploadRGBA(u.ren, d.img); err == nil {
			u.full.put(id, te)
			return true
		}
		return false
	}
	uploads := 0
	tryUp := func(i int) bool {
		if check(i) {
			uploads++
		}
		return uploads >= 2
	}
	if tryUp(u.cursor) {
		return
	}
	for d := 1; d <= 6; d++ {
		if tryUp(u.cursor+d) || tryUp(u.cursor-d) {
			return
		}
	}
}

/* ── viewer draw + transform ──────────────────────────────── */

func (u *ui) stageRect() sdl.Rect {
	w, h := u.win.GetSize()
	return sdl.Rect{X: 10, Y: 44, W: w - 20, H: h - 44 - 66}
}

func (u *ui) drawViewer() {
	s := u.shots[u.cursor]
	st := u.stageRect()

	if s.Kind == "video" {
		u.drawVideo(st)
		return
	}

	te := u.full.get(s.ID)
	if te == nil {
		// Keep the previous frame on screen while the new one decodes and
		// uploads — a full-screen flash per navigation reads as strobing.
		if u.lastTex != nil {
			dst := sdl.FRect{
				X: float32(float64(st.X) + u.tx), Y: float32(float64(st.Y) + u.ty),
				W: float32(float64(u.lastTex.w) * u.scale), H: float32(float64(u.lastTex.h) * u.scale),
			}
			u.ren.SetClipRect(&st)
			u.ren.CopyF(u.lastTex.tex, nil, &dst)
			u.ren.SetClipRect(nil)
		}
		d := u.pool.Get(s.ID)
		msg := "BUFFERING " + s.Base
		if d != nil && d.err != nil {
			msg = "FETCH FAILED — retrying " + s.Base
		}
		u.text(u.font, msg, colDim, st.X+st.W/2, st.Y+st.H-30, true)
		return
	}

	if u.curTexID != s.ID {
		u.mountTexture(s.ID, te, st)
		u.lastTex = te
	}
	dst := sdl.FRect{
		X: float32(float64(st.X) + u.tx), Y: float32(float64(st.Y) + u.ty),
		W: float32(float64(te.w) * u.scale), H: float32(float64(te.h) * u.scale),
	}
	u.ren.SetClipRect(&st)
	u.ren.CopyF(te.tex, nil, &dst)
	u.ren.SetClipRect(nil)

	// decision frame + badge
	if d := u.decisions[s.ID]; d != "" {
		c := colKeep
		if d == "reject" {
			c = colReject
		}
		u.ren.SetDrawColor(c.R, c.G, c.B, 255)
		u.ren.DrawRect(&st)
		u.text(u.font, strings.ToUpper(d), c, st.X+18, st.Y+14, false)
	}
	if u.scale > u.fit+1e-4 {
		u.text(u.font, fmt.Sprintf("%d%%", int(u.scale*100+0.5)), colAmber, st.X+st.W-70, st.Y+14, false)
	}
}

func (u *ui) mountTexture(id string, te *texEntry, st sdl.Rect) {
	u.curTexID = id
	u.natW, u.natH = te.w, te.h
	u.fit = minf(float64(st.W)/float64(te.w), float64(st.H)/float64(te.h))
	if u.fit > 1 {
		u.fit = 1
	}
	aspect := float64(te.w) / float64(te.h)
	if u.zoomMem != nil && absf(u.zoomMem.aspect-aspect) < 0.01 {
		u.scale = maxf(u.fit, minf(8, u.zoomMem.scale))
		u.tx = float64(st.W)/2 - u.zoomMem.cx*float64(te.w)*u.scale
		u.ty = float64(st.H)/2 - u.zoomMem.cy*float64(te.h)*u.scale
	} else {
		u.zoomMem = nil
		u.scale = u.fit
	}
	u.clampPan(st)
}

func (u *ui) clampPan(st sdl.Rect) {
	w := float64(u.natW) * u.scale
	h := float64(u.natH) * u.scale
	if w <= float64(st.W) {
		u.tx = (float64(st.W) - w) / 2
	} else {
		u.tx = minf(0, maxf(float64(st.W)-w, u.tx))
	}
	if h <= float64(st.H) {
		u.ty = (float64(st.H) - h) / 2
	} else {
		u.ty = minf(0, maxf(float64(st.H)-h, u.ty))
	}
	// persist zoom for same-aspect neighbours
	if u.scale > u.fit+1e-4 {
		u.zoomMem = &zoomMem{
			scale:  u.scale,
			cx:     (float64(st.W)/2 - u.tx) / u.scale / float64(u.natW),
			cy:     (float64(st.H)/2 - u.ty) / u.scale / float64(u.natH),
			aspect: float64(u.natW) / float64(u.natH),
		}
	} else {
		u.zoomMem = nil
	}
}

func (u *ui) zoomAt(px, py float64, newScale float64) {
	st := u.stageRect()
	newScale = maxf(u.fit, minf(8, newScale))
	ix := (px - u.tx) / u.scale
	iy := (py - u.ty) / u.scale
	u.tx = px - ix*newScale
	u.ty = py - iy*newScale
	u.scale = newScale
	u.clampPan(st)
}

/* ── header + strip ───────────────────────────────────────── */

func (u *ui) drawHeader() {
	w, _ := u.win.GetSize()
	u.fillRect(sdl.Rect{X: 0, Y: 0, W: w, H: 38}, colPanel)
	s := u.shots[u.cursor]
	u.text(u.font, fmt.Sprintf("%04d/%04d", u.cursor+1, len(u.shots)), colFG, 14, 10, false)
	name := fmt.Sprintf("%s / %s", s.Folder, s.Base)
	u.text(u.font, name, colFG, w/2, 10, true)
	keep, rej := 0, 0
	for _, d := range u.decisions {
		if d == "keep" {
			keep++
		} else {
			rej++
		}
	}
	u.text(u.font, fmt.Sprintf("K %d   X %d   · %d", keep, rej, len(u.shots)-keep-rej), colDim, w-260, 10, false)
	if states, have := u.app.ThumbProgress(); states != "" {
		total, healing := 0, 0
		for _, c := range states {
			if c != '-' {
				total++
			}
			if c == '2' {
				healing++
			}
		}
		if have < total {
			label := fmt.Sprintf("TH %d/%d", have, total)
			if healing > 0 {
				label += fmt.Sprintf(" · %d healing", healing)
			}
			u.text(u.fontSm, label, colDim, w-470, 12, false)
		}
	}
	if u.orients != "" {
		mdTotal, mdKnown := 0, 0
		for _, c := range u.orients {
			if c != '-' {
				mdTotal++
			}
			if c != '-' && c != '0' {
				mdKnown++
			}
		}
		if mdKnown < mdTotal {
			u.text(u.fontSm, fmt.Sprintf("MD %d/%d", mdKnown, mdTotal), colDim, w-640, 12, false)
		}
	}
	if u.camBulkSick || u.camPartSick {
		// Camera breaker: blink so a wedged link is unmissable mid-cull.
		label := "CAMERA SICK · POWER-CYCLE"
		if !u.camBulkSick {
			label = "CAMERA PARTIAL-READS SICK · POWER-CYCLE"
		}
		if u.frameN/30%2 == 0 {
			u.text(u.fontSm, label, colReject, w-870, 12, false)
		}
	}
}

const tickW, tickH, tickGap = 26, 40, 2

func (u *ui) drawStrip() {
	w, h := u.win.GetSize()
	stripY := h - 62
	u.fillRect(sdl.Rect{X: 0, Y: stripY, W: w, H: 62}, colPanel)

	pitch := int32(tickW + tickGap)
	visible := int(w/pitch) + 2
	first := u.cursor - visible/2
	for i := 0; i < visible; i++ {
		idx := first + i
		if idx < 0 || idx >= len(u.shots) {
			continue
		}
		s := u.shots[idx]
		x := int32(i)*pitch + (w-int32(visible)*pitch)/2
		r := sdl.Rect{X: x, Y: stripY + 8, W: tickW, H: tickH}
		u.fillRect(r, colTickBG)
		if tp, ok := u.app.ThumbPathIfReady(s.ID); ok {
			if te := u.thumbTex(s.ID, tp, u.orientAt(idx)); te != nil {
				// cover-fit the (upright) thumb into the tick
				src := coverSrc(te.w, te.h, tickW, tickH)
				u.ren.Copy(te.tex, &src, &r)
			}
		}
		// pipeline stripe: amber video, cyan decoded in GPU pipeline,
		// blue buffered on disk from the camera
		var stripe *sdl.Color
		if s.Kind == "video" {
			stripe = &colAmber
		} else if d := u.pool.Get(s.ID); d != nil && d.img != nil {
			stripe = &colDecoded
		} else if u.fetchStates[s.ID] == "ready" {
			stripe = &colBuffered
		}
		if stripe != nil {
			u.fillRect(sdl.Rect{X: x, Y: stripY + 8, W: tickW, H: 3}, *stripe)
		}
		// decision bar
		if d := u.decisions[s.ID]; d != "" {
			c := colKeep
			if d == "reject" {
				c = colReject
			}
			u.fillRect(sdl.Rect{X: x, Y: stripY + 8 + tickH - 4, W: tickW, H: 4}, c)
		}
		if idx == u.cursor {
			u.ren.SetDrawColor(colFG.R, colFG.G, colFG.B, 255)
			out := sdl.Rect{X: r.X - 2, Y: r.Y - 2, W: r.W + 4, H: r.H + 4}
			u.ren.DrawRect(&out)
		}
	}
	u.text(u.fontSm, "←→ nav   K keep   X reject   C clear   U undo   G next   T grid   I import   L video   Z 1:1   Space pause   ,/. seek   Q quit", colDim, w/2, h-16, true)
}

func coverSrc(tw, th, dw, dh int32) sdl.Rect {
	ta := float64(tw) / float64(th)
	da := float64(dw) / float64(dh)
	if ta > da { // source wider: crop sides
		cw := int32(float64(th) * da)
		return sdl.Rect{X: (tw - cw) / 2, Y: 0, W: cw, H: th}
	}
	ch := int32(float64(tw) / da)
	return sdl.Rect{X: 0, Y: (th - ch) / 2, W: tw, H: ch}
}

// orientAt is the shot's EXIF orientation per the server store (1 = upright
// or unknown). Thumb files carry no EXIF, so rotation is applied here.
func (u *ui) orientAt(idx int) int {
	if idx >= 0 && idx < len(u.orients) && u.orients[idx] > '1' && u.orients[idx] <= '8' {
		return int(u.orients[idx] - '0')
	}
	return 1
}

func (u *ui) thumbTex(id, path string, orient int) *texEntry {
	if te := u.thumbs.get(id); te != nil {
		if te.orient == orient {
			return te
		}
		u.thumbs.drop(id) // orientation arrived after caching: re-decode upright
	}
	if t, bad := u.thumbBad[id]; bad && time.Since(t) < 30*time.Second {
		return nil // corrupt on disk; recheck occasionally (sweep may replace it)
	}
	img, err := turbo.DecodeFile(path)
	if err != nil {
		if u.thumbBad == nil {
			u.thumbBad = map[string]time.Time{}
		}
		u.thumbBad[id] = time.Now()
		return nil
	}
	delete(u.thumbBad, id)
	te, err := uploadRGBA(u.ren, img.Normalize(orient))
	if err != nil {
		return nil
	}
	te.orient = orient
	u.thumbs.put(id, te)
	return te
}

/* ── input ────────────────────────────────────────────────── */

func (u *ui) handleEvent(ev sdl.Event) bool {
	switch e := ev.(type) {
	case *sdl.QuitEvent:
		return false
	case *sdl.TextInputEvent:
		if u.mode == modeImport {
			u.importText(e.GetText())
		}
		return true
	case *sdl.KeyboardEvent:
		if e.Type != sdl.KEYDOWN || u.shots == nil {
			return true
		}
		if u.mode == modeImport {
			u.importKey(e)
			return true
		}
		if u.mode == modeGrid {
			switch e.Keysym.Sym {
			case sdl.K_t, sdl.K_ESCAPE, sdl.K_RETURN:
				u.mode = modeViewer
				return true
			case sdl.K_UP:
				u.nav(u.cursor - u.gridCols())
				return true
			case sdl.K_DOWN:
				u.nav(u.cursor + u.gridCols())
				return true
			}
		}
		switch e.Keysym.Sym {
		case sdl.K_q:
			return false
		case sdl.K_ESCAPE:
			if u.scale > u.fit+1e-4 {
				u.scale = u.fit
				u.zoomMem = nil
				u.clampPan(u.stageRect())
			}
		case sdl.K_RIGHT:
			u.nav(u.cursor + 1)
		case sdl.K_LEFT, sdl.K_h:
			u.nav(u.cursor - 1)
		case sdl.K_HOME:
			u.nav(0)
		case sdl.K_END:
			u.nav(len(u.shots) - 1)
		case sdl.K_k:
			u.decide("keep")
		case sdl.K_x:
			u.decide("reject")
		case sdl.K_c:
			u.decide("")
		case sdl.K_u:
			u.undoLast()
		case sdl.K_g:
			u.nextUndecided()
		case sdl.K_t:
			if u.mode == modeGrid {
				u.mode = modeViewer
			} else {
				u.mode = modeGrid
			}
		case sdl.K_i:
			u.mode = modeImport
			sdl.StartTextInput()
		case sdl.K_l:
			s := u.shots[u.cursor]
			if s.Kind == "video" {
				u.app.EnsureVideo(s.ID)
			} else {
				u.nav(u.cursor + 1)
			}
		case sdl.K_r:
			s := u.shots[u.cursor]
			u.pool.mu.Lock()
			delete(u.pool.done, s.ID)
			u.pool.mu.Unlock()
		case sdl.K_COMMA:
			if u.mpv != nil && u.videoID != "" {
				u.mpv.Seek(-5)
			}
		case sdl.K_PERIOD:
			if u.mpv != nil && u.videoID != "" {
				u.mpv.Seek(5)
			}
		case sdl.K_z:
			st := u.stageRect()
			if u.scale > u.fit+1e-4 {
				u.scale = u.fit
			} else {
				u.zoomAt(float64(st.W)/2, float64(st.H)/2, 1)
			}
			u.clampPan(st)
		case sdl.K_SPACE:
			if u.shots[u.cursor].Kind == "video" && u.mpv != nil {
				u.mpv.SetPause(!u.mpv.Paused())
			} else {
				st := u.stageRect()
				if u.scale > u.fit+1e-4 {
					u.scale = u.fit
				} else {
					u.zoomAt(float64(st.W)/2, float64(st.H)/2, 1)
				}
				u.clampPan(st)
			}
		}
	case *sdl.MouseWheelEvent:
		if u.shots == nil {
			return true
		}
		if u.mode == modeGrid {
			u.gridTop -= int(e.Y) * 2
			if u.gridTop < 0 {
				u.gridTop = 0
			}
			return true
		}
		if u.curTexID == "" {
			return true
		}
		mx, my, _ := sdl.GetMouseState()
		st := u.stageRect()
		factor := 1.0 + 0.15*float64(e.Y)
		u.zoomAt(float64(mx-st.X), float64(my-st.Y), u.scale*factor)
	case *sdl.MouseButtonEvent:
		if e.Button == sdl.BUTTON_LEFT && u.shots != nil {
			if e.Type == sdl.MOUSEBUTTONDOWN {
				if u.mode == modeGrid {
					if idx := u.gridClick(e.X, e.Y); idx >= 0 {
						if idx == u.cursor {
							u.mode = modeViewer // second click on selected opens it
						}
						u.nav(idx)
					}
					return true
				}
				// filmstrip click-to-jump
				_, h := u.win.GetSize()
				if e.Y > h-62 {
					if idx := u.stripClick(e.X); idx >= 0 {
						u.nav(idx)
					}
					return true
				}
				// video seek-bar click
				s := u.shots[u.cursor]
				if s.Kind == "video" && u.mpv != nil && u.videoBar.W > 0 &&
					e.Y >= u.videoBar.Y-6 && e.Y <= u.videoBar.Y+u.videoBar.H+6 &&
					e.X >= u.videoBar.X && e.X <= u.videoBar.X+u.videoBar.W {
					_, dur := u.mpv.Position()
					frac := float64(e.X-u.videoBar.X) / float64(u.videoBar.W)
					u.mpv.Seek(frac*dur - func() float64 { p, _ := u.mpv.Position(); return p }())
					return true
				}
				if u.scale > u.fit+1e-4 {
					u.panning = true
					u.panStart = [2]int32{e.X, e.Y}
					u.panStartTx = [2]float64{u.tx, u.ty}
				}
			} else {
				u.panning = false
			}
		}
	case *sdl.MouseMotionEvent:
		if u.panning {
			u.tx = u.panStartTx[0] + float64(e.X-u.panStart[0])
			u.ty = u.panStartTx[1] + float64(e.Y-u.panStart[1])
			u.clampPan(u.stageRect())
		}
	}
	return true
}

func (u *ui) nav(i int) {
	if i < 0 || i >= len(u.shots) {
		return
	}
	if u.shots[u.cursor].Kind == "video" && (i >= len(u.shots) || u.shots[i].ID != u.shots[u.cursor].ID) {
		u.stopVideo()
	}
	u.cursor = i
	u.app.SetCursor(i)
}

// stripClick maps a click in the strip band to a shot index.
func (u *ui) stripClick(mx int32) int {
	w, _ := u.win.GetSize()
	pitch := int32(tickW + tickGap)
	visible := int(w/pitch) + 2
	first := u.cursor - visible/2
	off := (w - int32(visible)*pitch) / 2
	idx := first + int((mx-off)/pitch)
	if idx < 0 || idx >= len(u.shots) {
		return -1
	}
	return idx
}

func (u *ui) decide(d string) {
	s := u.shots[u.cursor]
	u.undo = append(u.undo, struct {
		idx  int
		prev string
	}{u.cursor, u.decisions[s.ID]})
	if d == "" {
		delete(u.decisions, s.ID)
	} else {
		u.decisions[s.ID] = d
	}
	go u.app.SetDecision(s.ID, d)
	if d != "" {
		u.nav(u.cursor + 1)
	}
}

func (u *ui) undoLast() {
	if len(u.undo) == 0 {
		return
	}
	last := u.undo[len(u.undo)-1]
	u.undo = u.undo[:len(u.undo)-1]
	id := u.shots[last.idx].ID
	if last.prev == "" {
		delete(u.decisions, id)
	} else {
		u.decisions[id] = last.prev
	}
	go u.app.SetDecision(id, last.prev)
	u.nav(last.idx)
}

func (u *ui) nextUndecided() {
	for d := 1; d <= len(u.shots); d++ {
		i := (u.cursor + d) % len(u.shots)
		if u.decisions[u.shots[i].ID] == "" {
			u.nav(i)
			return
		}
	}
}

/* ── drawing helpers ──────────────────────────────────────── */

func (u *ui) fillRect(r sdl.Rect, c sdl.Color) {
	u.ren.SetDrawColor(c.R, c.G, c.B, c.A)
	u.ren.FillRect(&r)
}

// text renders (with caching) and draws a string; centered when center=true.
func (u *ui) text(f *ttf.Font, s string, c sdl.Color, x, y int32, center bool) {
	if s == "" || f == nil {
		return
	}
	key := fmt.Sprintf("%p|%s|%d%d%d", f, s, c.R, c.G, c.B)
	te := u.texts.get(key)
	if te == nil {
		surf, err := f.RenderUTF8Blended(s, c)
		if err != nil {
			return
		}
		tex, err := u.ren.CreateTextureFromSurface(surf)
		w, h := surf.W, surf.H
		surf.Free()
		if err != nil {
			return
		}
		te = &texEntry{tex: tex, w: w, h: h}
		u.texts.put(key, te)
	}
	dst := sdl.Rect{X: x, Y: y, W: te.w, H: te.h}
	if center {
		dst.X -= te.w / 2
	}
	u.ren.Copy(te.tex, nil, &dst)
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func absf(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}

var _ = sort.Ints // reserved
