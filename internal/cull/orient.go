package cull

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/zack/fuji-tools/internal/jpegmeta"
	"github.com/zack/fuji-tools/internal/mtppart"
	"github.com/zack/fuji-tools/internal/photo"
)

// EXIF orientation store. Thumbnails (camera GetThumb payloads and locally
// generated ones alike) carry no EXIF, so portrait shots render sideways in
// the timeline and grid unless the shot's orientation is known out-of-band.
// Disk thumbs always stay in sensor orientation; rotation is applied at
// delivery (the GUI rotates pixels at texture decode, the HTTP handler
// re-encodes on the fly). Orientation is learned from:
//  1. full images already buffered on disk (startup backfill),
//  2. every image that lands in the buffer (harvest on promote),
//  3. a camera sweep of 64 KB file heads via batched partial reads —
//     guarded by a circuit breaker, because the X-H2S can answer partial
//     reads with stale response buffers instead of file bytes.

func (p *Prefetcher) orientPath() string {
	return filepath.Join(p.cache, "orientation.json")
}

func (p *Prefetcher) loadOrient() {
	raw, err := os.ReadFile(p.orientPath())
	if err != nil {
		return
	}
	m := map[string]uint8{}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for id, v := range m {
		if v >= 1 && v <= 8 && p.cat.Get(id) != nil {
			p.orient[id] = v
		}
	}
	if len(p.orient) > 0 {
		log.Printf("orientation: %d shots known (persisted)", len(p.orient))
	}
}

// orientFlusher persists the store shortly after new orientations arrive
// (bulk harvests would otherwise rewrite a ~19k-entry JSON per shot).
func (p *Prefetcher) orientFlusher() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for range tick.C {
		p.mu.Lock()
		closed := p.closed
		var raw []byte
		if p.orientDirty {
			raw, _ = json.Marshal(p.orient)
			p.orientDirty = false
		}
		p.mu.Unlock()
		if raw != nil {
			tmp := p.orientPath() + ".tmp"
			if os.WriteFile(tmp, raw, 0o644) == nil {
				_ = os.Rename(tmp, p.orientPath())
			}
		}
		if closed {
			return
		}
	}
}

// harvestOrient learns a photo's orientation from its buffered full image.
func (p *Prefetcher) harvestOrient(s *photo.Shot) {
	if s.Kind != "photo" {
		return
	}
	p.mu.Lock()
	_, known := p.orient[s.ID]
	p.mu.Unlock()
	if known {
		return
	}
	f, err := os.Open(p.displayPath(s))
	if err != nil {
		return
	}
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if n < 4 || head[0] != 0xFF || head[1] != 0xD8 {
		return
	}
	v := jpegmeta.Orientation(head[:n])
	p.mu.Lock()
	p.orient[s.ID] = uint8(v)
	p.orientDirty = true
	p.mu.Unlock()
}

// backfillOrient harvests every already-buffered image once at startup.
func (p *Prefetcher) backfillOrient() {
	p.mu.Lock()
	var todo []*photo.Shot
	for _, s := range p.cat.Shots {
		if s.Kind != "photo" {
			continue
		}
		if _, known := p.orient[s.ID]; known {
			continue
		}
		if st, ok := p.state[s.ID]; ok && st.Status == "ready" {
			todo = append(todo, s)
		}
	}
	p.mu.Unlock()
	for _, s := range todo {
		p.harvestOrient(s)
	}
	if len(todo) > 0 {
		log.Printf("orientation: backfilled %d buffered shots", len(todo))
	}
}

// OrientStates returns one byte per catalog shot: '1'-'8' known orientation,
// '0' unknown, '-' never applicable (videos).
func (p *Prefetcher) OrientStates() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	buf := make([]byte, len(p.cat.Shots))
	for i, s := range p.cat.Shots {
		switch v := p.orient[s.ID]; {
		case s.Kind != "photo":
			buf[i] = '-'
		case v >= 1 && v <= 8:
			buf[i] = '0' + v
		default:
			buf[i] = '0'
		}
	}
	return string(buf)
}

// OrientOf returns a shot's EXIF orientation, or 1 when unknown.
func (p *Prefetcher) OrientOf(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := p.orient[id]; v >= 2 && v <= 8 {
		return int(v)
	}
	return 1
}

/* ── camera header sweep ──────────────────────────────────── */

// orientBatchSize bounds one partial-read invocation. The aft session setup
// dominates (~4 s per invocation; the 64 KB heads themselves move at ~270
// files/s), so batches are large — 256 heads ≈ 16 MB ≈ one second of link
// time on top of the fixed setup cost.
const orientBatchSize = 256

// pickOrientBatchLocked selects photos with unknown orientation nearest the
// sweep origin, expanding outward in catalog order. Partial reads address
// objects by ID, so a batch may span camera folders freely.
func (p *Prefetcher) pickOrientBatchLocked(n int) []*photo.Shot {
	if !p.thumbRetryAt.IsZero() && time.Now().Before(p.thumbRetryAt) {
		return nil
	}
	needs := func(s *photo.Shot) bool {
		if s.Kind != "photo" || s.ObjectIDs[s.DisplayExt()] == "" {
			return false
		}
		_, known := p.orient[s.ID]
		return !known
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

// fetchOrientBatch pulls 64 KB heads for a batch in one partial-read session
// and parses orientations. Any response that is not a JPEG head trips the
// partial-read circuit breaker for the whole session — the X-H2S replays
// stale response buffers once its partial-read machinery degrades, and
// nothing it returns after that can be trusted.
func (p *Prefetcher) fetchOrientBatch(ctx context.Context, batch []*photo.Shot) {
	tmp, err := os.MkdirTemp(p.cache, "orient-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)

	reqs := make([]mtppart.PartReq, len(batch))
	for i, s := range batch {
		reqs[i] = mtppart.PartReq{
			ObjectID: s.ObjectIDs[s.DisplayExt()],
			Offset:   0,
			Size:     64 << 10,
			Dest:     filepath.Join(tmp, s.SafeID()+".bin"),
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second+time.Duration(len(batch))*500*time.Millisecond)
	runErr := mtppart.GetParts(cctx, reqs)
	canceled := ctx.Err() != nil || cctx.Err() != nil
	cancel()

	got, garbage := 0, 0
	p.mu.Lock()
	for i, s := range batch {
		head, err := os.ReadFile(reqs[i].Dest)
		if err != nil || len(head) == 0 {
			continue // not transferred (cancel/error); retry naturally
		}
		if head[0] != 0xFF || head[1] != 0xD8 {
			garbage++
			continue
		}
		p.orient[s.ID] = uint8(jpegmeta.Orientation(head))
		p.orientDirty = true
		got++
	}
	if garbage > 0 {
		p.markPartSickLocked()
		log.Printf("orientation: %d/%d partial reads returned non-JPEG data — camera partial reads are UNTRUSTWORTHY — pausing partial reads (power-cycle the camera; probing every 3m)", garbage, len(batch))
	}
	p.mu.Unlock()

	if runErr != nil && !canceled {
		p.mu.Lock()
		p.thumbRetryAt = time.Now().Add(15 * time.Second)
		p.mu.Unlock()
		log.Printf("orientation: batch: %v", runErr)
	} else if got > 0 {
		log.Printf("orientation: +%d from camera heads", got)
	}
}

/* ── head-heal sweep ──────────────────────────────────────── */

// The camera cannot serve thumbnails for shots past its thumbnail-DB
// capacity (the stale-buffer fragment bug), but every Fuji JPG embeds the
// same 160×120 preview in its EXIF header — a 128 KB head pull heals a
// thumbnail ~1000× cheaper than the old path (full 10 MB image pulled one
// per session, decoded and downscaled: ~0.3 shots/s vs ~150 heads/s).
const healHeadSize = 128 << 10

// pickHealBatchLocked selects camera-impossible shots not yet head-healed.
func (p *Prefetcher) pickHealBatchLocked(n int) []*photo.Shot {
	if !p.thumbRetryAt.IsZero() && time.Now().Before(p.thumbRetryAt) {
		return nil
	}
	needs := func(s *photo.Shot) bool {
		return s.Kind == "photo" && p.thumbs[s.ID] == thumbFailed &&
			!p.healTried[s.ID] && s.ObjectIDs[s.DisplayExt()] != ""
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

// fetchHealBatch pulls file heads for a batch and banks their EXIF-embedded
// thumbnails (plus orientation, for free). Shots whose heads carry no
// embedded thumbnail stay in the failed set for the full-image generator.
func (p *Prefetcher) fetchHealBatch(ctx context.Context, batch []*photo.Shot) {
	tmp, err := os.MkdirTemp(p.thumbDir, "heal-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)

	reqs := make([]mtppart.PartReq, len(batch))
	for i, s := range batch {
		reqs[i] = mtppart.PartReq{
			ObjectID: s.ObjectIDs[s.DisplayExt()],
			Offset:   0,
			Size:     healHeadSize,
			Dest:     filepath.Join(tmp, s.SafeID()+".bin"),
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second+time.Duration(len(batch))*500*time.Millisecond)
	runErr := mtppart.GetParts(cctx, reqs)
	canceled := ctx.Err() != nil || cctx.Err() != nil
	cancel()

	healed, bare, garbage := 0, 0, 0
	p.mu.Lock()
	for i, s := range batch {
		head, err := os.ReadFile(reqs[i].Dest)
		if err != nil || len(head) == 0 {
			continue // not transferred (cancel/error); retry naturally
		}
		if head[0] != 0xFF || head[1] != 0xD8 {
			garbage++
			continue
		}
		if _, known := p.orient[s.ID]; !known {
			p.orient[s.ID] = uint8(jpegmeta.Orientation(head))
			p.orientDirty = true
		}
		p.healTried[s.ID] = true
		th := jpegmeta.Thumbnail(head)
		if th == nil {
			bare++ // no embedded thumbnail: full-image generator's problem
			continue
		}
		tmpFile := p.ThumbPath(s) + ".heal"
		if os.WriteFile(tmpFile, th, 0o644) != nil || os.Rename(tmpFile, p.ThumbPath(s)) != nil {
			os.Remove(tmpFile)
			continue
		}
		p.thumbs[s.ID] = thumbHave
		delete(p.thumbStalls, s.ID)
		healed++
	}
	if healed > 0 {
		p.saveThumbFailedLocked()
	}
	if garbage > 0 {
		p.markPartSickLocked()
		log.Printf("thumbs: %d/%d heal heads returned non-JPEG data — camera partial reads are UNTRUSTWORTHY — pausing partial reads (power-cycle the camera; probing every 3m)", garbage, len(batch))
	}
	p.mu.Unlock()

	if runErr != nil && !canceled {
		p.mu.Lock()
		p.thumbRetryAt = time.Now().Add(15 * time.Second)
		p.mu.Unlock()
		log.Printf("thumbs: heal batch: %v", runErr)
	} else if healed > 0 || bare > 0 {
		log.Printf("thumbs: +%d healed from camera heads (%d without embedded thumbnails)", healed, bare)
	}
}

/* ── delivery-time rotation (HTTP thumbs) ─────────────────── */

// rotatedThumbJPEG re-encodes a thumbnail upright for the web UI. Thumbs are
// ~240 px, so decode+rotate+encode is ~1 ms — cheap enough per request, and
// browsers cache the result against an orientation-versioned URL.
func rotatedThumbJPEG(path string, orient int) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	up := normalizeRGBA(toRGBA(src), orient)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, up, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func toRGBA(src image.Image) *image.RGBA {
	if m, ok := src.(*image.RGBA); ok {
		return m
	}
	b := src.Bounds()
	m := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			m.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return m
}

// normalizeRGBA rewrites pixels upright per EXIF orientation (1-8).
func normalizeRGBA(m *image.RGBA, orient int) *image.RGBA {
	switch orient {
	case 2:
		return flipHRGBA(m)
	case 3:
		return rotRGBA(m, 2)
	case 4:
		return flipHRGBA(rotRGBA(m, 2))
	case 5:
		return flipHRGBA(rotRGBA(m, 1))
	case 6:
		return rotRGBA(m, 1)
	case 7:
		return flipHRGBA(rotRGBA(m, 3))
	case 8:
		return rotRGBA(m, 3)
	}
	return m
}

// rotRGBA rotates by quarter-turns clockwise (1, 2 or 3).
func rotRGBA(m *image.RGBA, quarters int) *image.RGBA {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	var out *image.RGBA
	if quarters == 2 {
		out = image.NewRGBA(image.Rect(0, 0, w, h))
	} else {
		out = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			px := m.PixOffset(x, y)
			var dx, dy int
			switch quarters {
			case 1:
				dx, dy = h-1-y, x
			case 2:
				dx, dy = w-1-x, h-1-y
			case 3:
				dx, dy = y, w-1-x
			}
			do := out.PixOffset(dx, dy)
			copy(out.Pix[do:do+4], m.Pix[px:px+4])
		}
	}
	return out
}

func flipHRGBA(m *image.RGBA) *image.RGBA {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			px := m.PixOffset(x, y)
			do := out.PixOffset(w-1-x, y)
			copy(out.Pix[do:do+4], m.Pix[px:px+4])
		}
	}
	return out
}
