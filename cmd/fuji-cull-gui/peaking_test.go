package main

import (
	"testing"

	"github.com/zack/fuji-tools/internal/turbo"
)

// An overlay is scaled for display only, so a lone hot pixel — a one-pixel
// edge is the whole signal peaking carries — has to survive the shrink. An
// averaging resize is what would lose it.
func TestShrinkMaxKeepsLoneEdge(t *testing.T) {
	const w, h = 400, 400
	img := &turbo.Image{Pix: make([]byte, w*h*4), W: w, H: h, NatW: w, NatH: h}
	img.Pix[(137*w+201)*4+3] = 250 // one edge pixel, nothing around it

	out := shrinkMax(img, 100, 100)
	if out.W > 100 || out.H > 100 {
		t.Fatalf("shrunk to %dx%d, want no more than 100x100", out.W, out.H)
	}
	if out.NatW != w || out.NatH != h {
		t.Errorf("native size %dx%d, want %dx%d", out.NatW, out.NatH, w, h)
	}
	var best byte
	for i := 3; i < len(out.Pix); i += 4 {
		if out.Pix[i] > best {
			best = out.Pix[i]
		}
	}
	if best != 250 {
		t.Errorf("strongest alpha after shrink = %d, want the original 250", best)
	}
}

// Nothing to shrink: an overlay already inside the box is passed straight
// through rather than copied.
func TestShrinkMaxLeavesSmallAlone(t *testing.T) {
	img := &turbo.Image{Pix: make([]byte, 40*30*4), W: 40, H: 30}
	if out := shrinkMax(img, 100, 100); out != img {
		t.Errorf("overlay smaller than the box was rebuilt at %dx%d", out.W, out.H)
	}
}

// A blank frame stays blank: an all-zero overlay must not pick up tint.
func TestShrinkMaxKeepsBlankBlank(t *testing.T) {
	img := &turbo.Image{Pix: make([]byte, 400*400*4), W: 400, H: 400}
	out := shrinkMax(img, 50, 50)
	for i, b := range out.Pix {
		if b != 0 {
			t.Fatalf("byte %d of a blank overlay = %d, want 0", i, b)
		}
	}
}
