package main

import (
	"sync"
	"time"
	"unsafe"

	"github.com/veandco/go-sdl2/sdl"

	"github.com/zack/fuji-tools/internal/turbo"
)

// Focus peaking for the desktop viewer: a toggleable overlay painting the
// in-focus edges of the frame on screen, so "did this one nail focus, and on
// the right thing?" is a glance instead of a zoom-and-hunt.
//
// The GUI already holds every frame as decoded RGBA (turbo.Image) on its way to
// an SDL texture, so this is a pass over pixels we've already paid for — no
// engine round-trip, and it measures the FULL-resolution frame, which is what
// makes it trustworthy for critical focus. That resolution is the point and
// not negotiable: a frame fitted to the stage has already averaged away the
// difference between sharp and nearly-sharp, which is the only thing peaking
// is being asked. What it costs — a quarter-second Sobel pass over 26 MP — is
// paid on a worker instead of the render thread (see peaker), because paying
// it inline froze the UI for a dozen frames every time the cursor moved.

// peakTint is the overlay colour: saturated red, which reads far more clearly
// against real photo content than the cyan this started as. It does sit near
// the reject hue, but peaking is a transient overlay you switch on deliberately
// rather than a persistent per-shot mark, so the two never share a surface.
var peakTint = struct{ R, G, B byte }{0xFF, 0x18, 0x18}

// peakCutPct is how strong an edge must be to light up, as a percentage of the
// maximum possible Sobel response. Tuned so a sharp frame shows structure and a
// soft one stays nearly bare — the contrast between the two is the whole point.
// Integer percent rather than a float fraction: the threshold is compared
// against integer magnitudes, so there's no reason to leave the int domain.
const peakCutPct = 14

// edgeOverlay returns a transparent RGBA image with the frame's strong edges
// painted in peakTint, ready to upload as a texture and blend over the photo.
func edgeOverlay(img *turbo.Image) *turbo.Image {
	w, h := img.W, img.H
	out := &turbo.Image{Pix: make([]byte, w*h*4), W: w, H: h, NatW: img.NatW, NatH: img.NatH}
	if w < 3 || h < 3 {
		return out
	}
	// Luminance plane first: Sobel on grey is one pass instead of three, and
	// focus is a luminance property anyway.
	lum := make([]byte, w*h)
	for i, p := 0, 0; i < w*h; i, p = i+1, p+4 {
		// integer Rec.601 — the fractional precision buys nothing here
		lum[i] = byte((uint32(img.Pix[p])*299 + uint32(img.Pix[p+1])*587 + uint32(img.Pix[p+2])*114) / 1000)
	}

	// Sobel magnitude, approximated as |gx|+|gy| (cheaper than the hypot and
	// indistinguishable once thresholded).
	const maxResp = 4 * 255 // |gx|+|gy| ceiling for this kernel pair
	cut := maxResp * peakCutPct / 100
	for y := 1; y < h-1; y++ {
		row := y * w
		up, dn := row-w, row+w
		for x := 1; x < w-1; x++ {
			tl, tc, tr := int(lum[up+x-1]), int(lum[up+x]), int(lum[up+x+1])
			ml, mr := int(lum[row+x-1]), int(lum[row+x+1])
			bl, bc, br := int(lum[dn+x-1]), int(lum[dn+x]), int(lum[dn+x+1])
			gx := (tr + 2*mr + br) - (tl + 2*ml + bl)
			gy := (bl + 2*bc + br) - (tl + 2*tc + tr)
			if gx < 0 {
				gx = -gx
			}
			if gy < 0 {
				gy = -gy
			}
			mag := gx + gy
			if mag < cut {
				continue
			}
			// Ramp alpha above the cut so the strongest edges read hardest,
			// rather than everything past the threshold looking identical.
			a := (mag - cut) * 255 / (maxResp - cut)
			if a > 255 {
				a = 255
			}
			o := (row + x) * 4
			out.Pix[o] = peakTint.R
			out.Pix[o+1] = peakTint.G
			out.Pix[o+2] = peakTint.B
			out.Pix[o+3] = byte(a)
		}
	}
	return out
}

// uploadOverlay uploads an edge overlay as an alpha-blended texture. Unlike
// uploadRGBA (which is BLENDMODE_NONE for opaque frames) this must blend, or it
// would paint an opaque black rectangle over the photo.
func uploadOverlay(r *sdl.Renderer, img *turbo.Image) (*texEntry, error) {
	tex, err := r.CreateTexture(uint32(sdl.PIXELFORMAT_ABGR8888), sdl.TEXTUREACCESS_STATIC,
		int32(img.W), int32(img.H))
	if err != nil {
		return nil, err
	}
	if err := tex.Update(nil, unsafe.Pointer(&img.Pix[0]), img.W*4); err != nil {
		tex.Destroy()
		return nil, err
	}
	tex.SetBlendMode(sdl.BLENDMODE_BLEND)
	return &texEntry{tex: tex, w: int32(img.W), h: int32(img.H),
		natW: int32(img.NatW), natH: int32(img.NatH)}, nil
}

// shrinkMax fits an overlay into a box by keeping the STRONGEST edge in each
// block rather than the average. Averaging is how a one-pixel-wide edge —
// exactly what peaking exists to show — disappears, so the measurement stays
// at full resolution and only the picture of it is scaled down. The screen
// cannot show more than the box anyway, and an overlay is the same size in
// memory as the frame it covers, so leaving it at 26 MP would hand the render
// thread a second 99 MB upload per shot.
func shrinkMax(img *turbo.Image, boxW, boxH int) *turbo.Image {
	if boxW <= 0 || boxH <= 0 || (img.W <= boxW && img.H <= boxH) {
		return img
	}
	k := 2
	for (img.W+k-1)/k > boxW || (img.H+k-1)/k > boxH {
		k++
	}
	ow, oh := (img.W+k-1)/k, (img.H+k-1)/k
	out := &turbo.Image{Pix: make([]byte, ow*oh*4), W: ow, H: oh, NatW: img.NatW, NatH: img.NatH}
	for oy := 0; oy < oh; oy++ {
		for ox := 0; ox < ow; ox++ {
			var a byte
			for y := oy * k; y < (oy+1)*k && y < img.H; y++ {
				row := y * img.W
				for x := ox * k; x < (ox+1)*k && x < img.W; x++ {
					if v := img.Pix[(row+x)*4+3]; v > a {
						a = v
					}
				}
			}
			if a == 0 {
				continue
			}
			o := (oy*ow + ox) * 4
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = peakTint.R, peakTint.G, peakTint.B, a
		}
	}
	return out
}

/* ── building overlays off the render thread ──────────────── */

// peaker builds one overlay at a time on a worker goroutine. The render
// thread never blocks on it: it says which shot it wants and, on some later
// frame, finds the overlay waiting. Overlays are kept as images rather than
// only as textures so that an evicted texture costs an upload to restore
// rather than another Sobel pass.
type peaker struct {
	pool *decodePool

	mu    sync.Mutex
	want  string
	box   [2]int
	built map[string]*turbo.Image
	order []string // insertion order, for a bounded cache
}

// peakCache is how many built overlays to keep. Small on purpose: each one is
// megabytes, and the only ones that get drawn are the shot on screen and
// whatever the cursor just left.
const peakCache = 3

func newPeaker(pool *decodePool) *peaker {
	p := &peaker{pool: pool, built: map[string]*turbo.Image{}}
	go p.worker()
	return p
}

// Want names the shot whose overlay is needed and the box it gets drawn into,
// and returns the overlay if it is already built. Never blocks.
func (p *peaker) Want(id string, boxW, boxH int) *turbo.Image {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.want, p.box = id, [2]int{boxW, boxH}
	return p.built[id]
}

func (p *peaker) worker() {
	for {
		p.mu.Lock()
		id, box := p.want, p.box
		have := id == "" || p.built[id] != nil
		p.mu.Unlock()
		if have {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// Wait for the full-resolution decode the viewer asked for on our
		// behalf; a fitted one would make a soft frame look sharp.
		d := p.pool.Get(id)
		if d == nil || d.img == nil || d.err != nil || !d.full {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		ov := shrinkMax(edgeOverlay(d.img), box[0], box[1])
		p.mu.Lock()
		if _, dup := p.built[id]; !dup {
			p.built[id] = ov
			p.order = append(p.order, id)
			for len(p.order) > peakCache {
				delete(p.built, p.order[0])
				p.order = p.order[1:]
			}
		}
		p.mu.Unlock()
	}
}

// peakTex returns the peaking overlay for a shot, or nil while one is still
// being built — the caller simply draws no overlay that redraw, and picks it
// up a few frames later. All this does on the render thread is the upload.
func (u *ui) peakTex(id string, st sdl.Rect) *texEntry {
	if te := u.peaks.get(id); te != nil {
		return te
	}
	ov := u.peaker.Want(id, int(st.W), int(st.H))
	if ov == nil {
		return nil
	}
	te, err := uploadOverlay(u.ren, ov)
	if err != nil {
		return nil
	}
	u.peaks.put(id, te)
	return te
}
