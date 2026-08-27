package main

import (
	"testing"

	"github.com/zack/fuji-tools/internal/turbo"
)

// bars builds a w x h thumbnail with `pad` black rows top and bottom and
// picture everywhere else — the shape Fuji writes for a 3:2 frame.
func bars(w, h, pad int) *turbo.Image {
	img := &turbo.Image{Pix: make([]byte, w*h*4), W: w, H: h}
	for y := pad; y < h-pad; y++ {
		for x := 0; x < w; x++ {
			p := (y*w + x) * 4
			img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = 0x80, 0x90, 0x70, 0xFF
		}
	}
	return img
}

func TestThumbContentTrimsBars(t *testing.T) {
	const pad = 7
	got := thumbContent(bars(160, 120, pad))
	if got.W != 160 {
		t.Fatalf("content = %+v, want full width", got)
	}
	// Every bar row gone, and no more than the one blend row of picture with
	// it: the placeholder must not visibly crop the frame.
	if int(got.Y) < pad || int(got.Y) > pad+1 {
		t.Errorf("top edge at y=%d, want %d or %d", got.Y, pad, pad+1)
	}
	if end := int(got.Y + got.H); end > 120-pad || end < 120-pad-1 {
		t.Errorf("bottom edge at y=%d, want %d or %d", end, 120-pad-1, 120-pad)
	}
}

// A thumbnail with no padding is used whole.
func TestThumbContentKeepsFullFrame(t *testing.T) {
	got := thumbContent(bars(160, 120, 0))
	if got.Y != 0 || got.H != 120 || got.W != 160 {
		t.Errorf("content = %+v, want the whole 160x120", got)
	}
}

// A dark photograph is a photograph. Trimming stops well before it could eat
// a night sky, and a frame that is black all the way through is left alone
// rather than collapsing to nothing.
func TestThumbContentLeavesDarkPhotos(t *testing.T) {
	if got := thumbContent(bars(160, 120, 40)); got.H != 120 {
		t.Errorf("heavily dark thumb trimmed to %+v; expected it left whole", got)
	}
	black := &turbo.Image{Pix: make([]byte, 160*120*4), W: 160, H: 120}
	if got := thumbContent(black); got.H != 120 || got.W != 160 {
		t.Errorf("all-black thumb = %+v, want the whole texture", got)
	}
}
