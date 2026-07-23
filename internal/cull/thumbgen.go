package cull

import (
	"image"
	"image/jpeg"
	"log"
	"os"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/zack/fuji-tools/internal/photo"
)

// localThumbGen generates missing thumbnails from full images already
// buffered on disk — zero camera traffic, so ordinary culling repairs the
// thumbnail cache as a side effect. Runs whenever the prefetcher is alive.
func (p *Prefetcher) localThumbGen() {
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		var target *photo.Shot
		for id, st := range p.state {
			if st.Status != "ready" {
				continue
			}
			s := p.cat.Get(id)
			// Handle failed shots too: for the fragment-thumb files the
			// camera can NEVER produce a thumbnail — local generation from
			// the buffered full image is their only path.
			if s == nil || s.Kind != "photo" || p.thumbs[s.ID] == thumbHave {
				continue
			}
			target = s
			break
		}
		// No buffered work: demand the full image of the nearest shot whose
		// thumbnail the camera cannot provide (fragment-thumb files), so the
		// normal image pipeline feeds the generator. One at a time, viewport-
		// steered, capped by per-shot attempts. Paused while bulk reads are
		// sick — these demands would hammer garbage transfers forever.
		if target == nil && !p.bulkSickLocked() {
			// Pipeline up to 3 demands so image transfer overlaps with
			// generation; the healing marches outward on its own — browsing
			// only re-prioritizes it.
			origin := p.thumbOriginLocked()
			queued := 0
			for d := 0; d < len(p.cat.Shots) && queued < 3; d++ {
				for _, i := range []int{origin + d, origin - d} {
					if queued >= 3 || i < 0 || i >= len(p.cat.Shots) || (d == 0 && i != origin) {
						continue
					}
					s := p.cat.Shots[i]
					if s.Kind != "photo" || p.thumbs[s.ID] != thumbFailed || p.thumbStalls[s.ID] >= 4 {
						continue
					}
					if st, ok := p.state[s.ID]; ok && (st.Status == "ready" || st.Status == "fetching") {
						queued++ // already inbound
						continue
					}
					delete(p.state, s.ID)
					p.demand[s.ID] = true
					queued++
				}
			}
		}
		p.mu.Unlock()
		if target == nil {
			p.cond.Broadcast()
			continue
		}
		if err := p.generateThumb(target); err != nil {
			log.Printf("thumbgen: %s: %v", target.ID, err)
			p.mu.Lock()
			p.thumbStalls[target.ID]++ // avoid retry loops on undecodable files
			if p.thumbStalls[target.ID] >= 2 {
				p.thumbs[target.ID] = thumbFailed
			}
			p.mu.Unlock()
		}
	}
}

// localThumbSweep generates thumbnails for the whole catalog directly from the
// backend's source files (the dir backend exposes every file), nearest the
// grid viewport first and re-steering as the cursor moves. No camera, no
// partial reads — the simulator / fake-backend thumbnail path.
func (p *Prefetcher) localThumbSweep() {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		origin := p.thumbOriginLocked()
		idx := -1
		for d := 0; d < len(p.cat.Shots) && idx < 0; d++ {
			for _, i := range []int{origin + d, origin - d} {
				if i < 0 || i >= len(p.cat.Shots) || (d == 0 && i != origin) {
					continue
				}
				s := p.cat.Shots[i]
				if s.Kind == "photo" && p.thumbs[s.ID] != thumbHave && p.thumbStalls[s.ID] < 2 {
					idx = i
					break
				}
			}
		}
		p.mu.Unlock()
		if idx < 0 {
			<-tick.C // swept clean; idle until the cursor moves or shots change
			continue
		}
		s := p.cat.Shots[idx]
		src, ok := p.backend.LocalPath(s, s.DisplayExt())
		if !ok {
			p.mu.Lock()
			p.thumbs[s.ID] = thumbFailed
			p.thumbStalls[s.ID] = 2
			p.mu.Unlock()
			continue
		}
		if err := p.generateThumbFrom(s, src); err != nil {
			log.Printf("localthumb: %s: %v", s.ID, err)
			p.mu.Lock()
			p.thumbStalls[s.ID]++
			if p.thumbStalls[s.ID] >= 2 {
				p.thumbs[s.ID] = thumbFailed
			}
			p.mu.Unlock()
		}
	}
}

// generateThumb decodes the buffered full image and writes a 240px-wide
// thumbnail to the standard thumb cache path.
func (p *Prefetcher) generateThumb(s *photo.Shot) error {
	return p.generateThumbFrom(s, p.displayPath(s))
}

// generateThumbFrom decodes srcPath and writes a 240px-wide thumbnail to the
// shot's standard thumb cache path.
func (p *Prefetcher) generateThumbFrom(s *photo.Shot, srcPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	b := src.Bounds()
	w := 240
	h := b.Dy() * w / b.Dx()
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)

	tmp := p.ThumbPath(s) + ".gen"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: 82}); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	if err := os.Rename(tmp, p.ThumbPath(s)); err != nil {
		os.Remove(tmp)
		return err
	}
	p.mu.Lock()
	wasFailed := p.thumbs[s.ID] == thumbFailed
	p.thumbs[s.ID] = thumbHave
	if wasFailed {
		p.saveThumbFailedLocked()
	}
	p.mu.Unlock()
	return nil
}
