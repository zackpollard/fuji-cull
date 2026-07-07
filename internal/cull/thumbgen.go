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
			if s == nil || s.Kind != "photo" || p.thumbs[s.ID] != thumbMissing {
				continue
			}
			target = s
			break
		}
		p.mu.Unlock()
		if target == nil {
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

// generateThumb decodes the buffered full image and writes a 240px-wide
// thumbnail to the standard thumb cache path.
func (p *Prefetcher) generateThumb(s *photo.Shot) error {
	f, err := os.Open(p.displayPath(s))
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
	p.thumbs[s.ID] = thumbHave
	p.mu.Unlock()
	return nil
}
