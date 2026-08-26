package turbo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// synth encodes a w x h JPEG with enough structure that scaling is visible.
func synth(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A frame is letterboxed into the stage, so the rendition only has to cover
// the fitted size — the box's other axis is never filled and must not drag a
// tall frame back up to full resolution.
func TestScaledSizeFitsLetterbox(t *testing.T) {
	cases := []struct{ w, h, boxW, boxH int }{
		{6240, 4160, 3820, 2050}, // landscape on a 4K stage
		{4160, 6240, 3820, 2050}, // portrait: limited by height alone
		{6240, 4160, 1200, 800},  // small window
	}
	for _, c := range cases {
		gw, gh := scaledSize(c.w, c.h, c.boxW, c.boxH)
		fit := min(float64(c.boxW)/float64(c.w), float64(c.boxH)/float64(c.h))
		needW, needH := float64(c.w)*fit, float64(c.h)*fit
		if float64(gw) < needW || float64(gh) < needH {
			t.Errorf("%dx%d in %dx%d: got %dx%d, short of the fitted %.0fx%.0f",
				c.w, c.h, c.boxW, c.boxH, gw, gh, needW, needH)
		}
		if gw*gh >= c.w*c.h {
			t.Errorf("%dx%d in %dx%d: got %dx%d — no pixels saved",
				c.w, c.h, c.boxW, c.boxH, gw, gh)
		}
	}
}

// A box at least as big as the frame must not ask for pixels the file has
// not got, and no box at all means the file's own size.
func TestScaledSizeNeverUpscales(t *testing.T) {
	for _, c := range [][4]int{{800, 600, 0, 0}, {800, 600, 4000, 3000}, {800, 600, 800, 600}} {
		if gw, gh := scaledSize(c[0], c[1], c[2], c[3]); gw != c[0] || gh != c[1] {
			t.Errorf("scaledSize(%v) = %dx%d, want native %dx%d", c, gw, gh, c[0], c[1])
		}
	}
}

// The decoded frame reports the size of the photograph, not of the rendition:
// the viewer lays out and zooms from that, so it must survive scaling.
func TestDecodeScaledReportsNativeSize(t *testing.T) {
	data := synth(t, 1600, 1200)
	small, err := DecodeScaled(data, 400, 300)
	if err != nil {
		t.Fatal(err)
	}
	if small.NatW != 1600 || small.NatH != 1200 {
		t.Errorf("native size = %dx%d, want 1600x1200", small.NatW, small.NatH)
	}
	if small.W >= 1600 {
		t.Errorf("decoded %dx%d — expected a smaller rendition", small.W, small.H)
	}
	if got, want := len(small.Pix), small.W*small.H*4; got != want {
		t.Errorf("pixel buffer %d bytes, want %d", got, want)
	}
	full, err := DecodeScaled(data, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if full.W != 1600 || full.NatW != 1600 {
		t.Errorf("unscaled decode = %dx%d (native %dx%d), want 1600x1200", full.W, full.H, full.NatW, full.NatH)
	}
}
