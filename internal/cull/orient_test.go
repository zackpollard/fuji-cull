package cull

import (
	"image"
	"testing"

	"github.com/zack/fuji-tools/internal/jpegmeta"
)

// synthetic JPEG head: SOI + APP1/Exif carrying a single orientation tag.
func exifHead(orient byte) []byte {
	tiff := []byte{
		'M', 'M', 0, 42, // big-endian TIFF
		0, 0, 0, 8, // IFD0 at offset 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, // tag 0x0112, type SHORT
		0, 0, 0, 1, // count 1
		0, orient, 0, 0, // value
		0, 0, 0, 0, // next IFD
	}
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}
	return append(seg, app1...)
}

func TestJpegmetaOrientation(t *testing.T) {
	for _, want := range []int{1, 3, 6, 8} {
		if got := jpegmeta.Orientation(exifHead(byte(want))); got != want {
			t.Errorf("orientation %d parsed as %d", want, got)
		}
	}
	if got := jpegmeta.Orientation([]byte{0xFF, 0xD8, 0xFF, 0xDA, 0, 4, 0, 0}); got != 1 {
		t.Errorf("no-EXIF jpeg parsed as %d, want 1", got)
	}
}

func TestJpegmetaRAF(t *testing.T) {
	// RAF header: FUJIFILM magic, embedded-JPEG offset big-endian at byte 84
	jpg := exifHead(6)
	raf := make([]byte, 148)
	copy(raf, "FUJIFILMCCD-RAW ")
	raf[84], raf[85], raf[86], raf[87] = 0, 0, 0, 148
	raf = append(raf, jpg...)
	if got := jpegmeta.Orientation(raf); got != 6 {
		t.Errorf("RAF-embedded orientation parsed as %d, want 6", got)
	}
}

func TestNormalizeRGBA(t *testing.T) {
	// 2x1 image: red at (0,0), blue at (1,0)
	m := image.NewRGBA(image.Rect(0, 0, 2, 1))
	copy(m.Pix[m.PixOffset(0, 0):], []byte{255, 0, 0, 255})
	copy(m.Pix[m.PixOffset(1, 0):], []byte{0, 0, 255, 255})

	// orientation 6 = rotate 90° CW: red should land at top-right of a 1x2
	r := normalizeRGBA(m, 6)
	if r.Bounds().Dx() != 1 || r.Bounds().Dy() != 2 {
		t.Fatalf("orient 6: got %v, want 1x2", r.Bounds())
	}
	if r.Pix[r.PixOffset(0, 0)] != 255 {
		t.Errorf("orient 6: red not at top after CW rotation")
	}
	if r.Pix[r.PixOffset(0, 1)+2] != 255 {
		t.Errorf("orient 6: blue not at bottom after CW rotation")
	}

	// orientation 8 = rotate 270° CW: red should land at bottom-left
	r = normalizeRGBA(m, 8)
	if r.Bounds().Dx() != 1 || r.Bounds().Dy() != 2 {
		t.Fatalf("orient 8: got %v, want 1x2", r.Bounds())
	}
	if r.Pix[r.PixOffset(0, 1)] != 255 {
		t.Errorf("orient 8: red not at bottom after CCW rotation")
	}

	// orientation 3 = 180°: red and blue swap
	r = normalizeRGBA(m, 3)
	if r.Bounds().Dx() != 2 || r.Bounds().Dy() != 1 {
		t.Fatalf("orient 3: got %v, want 2x1", r.Bounds())
	}
	if r.Pix[r.PixOffset(1, 0)] != 255 {
		t.Errorf("orient 3: red not at right after 180 rotation")
	}
}
