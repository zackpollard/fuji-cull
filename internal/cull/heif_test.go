package cull

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zack/fuji-tools/internal/photo"
)

func TestNeedsConvert(t *testing.T) {
	for ext, want := range map[string]bool{
		"JPG": false, "MOV": false, "MP4": false, "": false,
		"RAF": true, "HIF": true, "HEIC": true,
	} {
		if got := needsConvert(ext); got != want {
			t.Errorf("needsConvert(%q) = %v, want %v", ext, got, want)
		}
	}
}

// A HEIF is ISO-BMFF: a box length, then the "ftyp" type. Validating one as a
// JPEG would reject every pulled file; validating it loosely would let the
// camera's stale-buffer garbage through.
func TestMediaValidHEIF(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	heif := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic0000heic")...)
	if !mediaValid(write("a.heic", heif), "heif") {
		t.Error("a real HEIF head was rejected")
	}
	stale := []byte("STALEBUF" + "0123456789abcdef")
	if mediaValid(write("b.heic", stale), "heif") {
		t.Error("stale-buffer garbage passed as HEIF")
	}
	jpegHead := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 20)...)
	if mediaValid(write("c.heic", jpegHead), "heif") {
		t.Error("a JPEG passed as HEIF")
	}
}

// The display file for a HEIF shot is the JPEG we derive, so it has no
// camera-verbatim size to check against — same as a RAF-only shot.
func TestVerbatimSizeSkipsDerivedDisplayFiles(t *testing.T) {
	heifShot := &photo.Shot{Kind: "photo", Files: map[string]string{"HIF": "DSCF0001.HIF"},
		Sizes: map[string]int64{"HIF": 3891712}}
	if got := verbatimSize(heifShot); got != 0 {
		t.Errorf("verbatimSize(HEIF-only) = %d, want 0", got)
	}
	jpgShot := &photo.Shot{Kind: "photo", Files: map[string]string{"JPG": "DSCF0001.JPG"},
		Sizes: map[string]int64{"JPG": 12345}}
	if got := verbatimSize(jpgShot); got != 12345 {
		t.Errorf("verbatimSize(JPG) = %d, want 12345", got)
	}
}

// A HEIF head is ISO-BMFF, not a JPEG. mediaHead is the guard that decides a
// partial read returned stale-buffer garbage, so misreading one as garbage
// trips the camera-sick breaker on a perfectly healthy camera.
func TestMediaHeadAcceptsHEIF(t *testing.T) {
	heif := append([]byte{0, 0, 0, 0x18}, []byte("ftypheic0000heic")...)
	if !mediaHead(heif) {
		t.Error("HEIF head rejected — this trips the partial-read breaker")
	}
	mov := append([]byte{0, 0, 0, 0x14}, []byte("ftypqt  0000")...)
	if !mediaHead(mov) {
		t.Error("MOV head rejected")
	}
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE1, 0, 0, 0, 0}
	if !mediaHead(jpg) {
		t.Error("JPEG head rejected")
	}
	// what the guard exists for: replayed buffers must still be caught
	if mediaHead([]byte("STALEBUF0123456789")) {
		t.Error("stale-buffer garbage accepted as media")
	}
	if mediaHead([]byte{0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Error("zero bytes accepted as media")
	}
}

// A raw+JPEG pair looks identical to a lone JPEG in the viewer, because the
// camera's own rendering is what gets displayed. AltExt is what lets the UI
// say otherwise.
func TestAltExt(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"heif+raf", map[string]string{"HEIC": "a.HEIC", "RAF": "a.RAF"}, "RAF"},
		{"jpg+raf", map[string]string{"JPG": "a.JPG", "RAF": "a.RAF"}, "RAF"},
		{"jpg only", map[string]string{"JPG": "a.JPG"}, ""},
		{"heif only", map[string]string{"HEIC": "a.HEIC"}, ""},
		// a lone raw is already the thing on display, so nothing rides along
		{"raf only", map[string]string{"RAF": "a.RAF"}, ""},
	}
	for _, c := range cases {
		if got := AltExt(&photo.Shot{Kind: "photo", Files: c.files}); got != c.want {
			t.Errorf("%s: AltExt = %q, want %q", c.name, got, c.want)
		}
	}
	v := &photo.Shot{Kind: "video", Files: map[string]string{"MOV": "a.MOV", "RAF": "a.RAF"}}
	if got := AltExt(v); got != "" {
		t.Errorf("video AltExt = %q, want empty", got)
	}
}
