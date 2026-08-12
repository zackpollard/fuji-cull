package jpegmeta

import (
	"encoding/binary"
	"testing"
)

// buildTIFF assembles a little-endian TIFF whose IFD chain carries one
// JPEGInterchangeFormat/Length pair per entry in previews, mirroring how a
// TIFF-based raw hangs a thumbnail, a screen preview and a full-resolution
// JpgFromRaw off successive IFDs.
func buildTIFF(previews [][2]uint32) []byte {
	const entrySize, ifdSize = 12, 2 + 2*12 + 4 // two entries per IFD
	buf := make([]byte, 8+ifdSize*len(previews))
	copy(buf, "II*\x00")
	binary.LittleEndian.PutUint32(buf[4:], 8)

	for i, p := range previews {
		ifd := 8 + i*ifdSize
		binary.LittleEndian.PutUint16(buf[ifd:], 2) // entry count
		for e, tag := range []uint16{0x0201, 0x0202} {
			off := ifd + 2 + e*entrySize
			binary.LittleEndian.PutUint16(buf[off:], tag)
			binary.LittleEndian.PutUint16(buf[off+2:], 4) // LONG
			binary.LittleEndian.PutUint32(buf[off+4:], 1)
			binary.LittleEndian.PutUint32(buf[off+8:], p[e])
		}
		next := uint32(0)
		if i+1 < len(previews) {
			next = uint32(ifd + ifdSize)
		}
		binary.LittleEndian.PutUint32(buf[ifd+2+2*entrySize:], next)
	}
	return buf
}

func TestTIFFPreviewPicksLargest(t *testing.T) {
	// IFD0 screen preview, IFD1 thumbnail, IFD2 full-res — the ARW shape.
	head := buildTIFF([][2]uint32{{196770, 147441}, {43956, 4940}, {348160, 3258754}})
	off, length, ok := TIFFPreview(head)
	if !ok || off != 348160 || length != 3258754 {
		t.Errorf("TIFFPreview = (%d, %d, %v), want (348160, 3258754, true)", off, length, ok)
	}
}

func TestTIFFPreviewRejectsNonTIFF(t *testing.T) {
	for name, data := range map[string][]byte{
		"jpeg":  {0xFF, 0xD8, 0xFF, 0xE1, 0, 0, 0, 0},
		"raf":   []byte("FUJIFILMCCD-RAW 0201FF12345678"),
		"empty": {},
		"short": {'I', 'I'},
	} {
		if _, _, ok := TIFFPreview(data); ok {
			t.Errorf("TIFFPreview(%s) ok = true, want false", name)
		}
	}
}

// A corrupt next-IFD pointer must not spin: pointers run forward, so one that
// doesn't ends the walk.
func TestTIFFPreviewTerminatesOnSelfReferentialIFD(t *testing.T) {
	head := buildTIFF([][2]uint32{{100, 2000}, {200, 3000}})
	binary.LittleEndian.PutUint32(head[8+2+2*12:], 8) // IFD0 points at itself
	off, length, ok := TIFFPreview(head)
	if !ok || off != 100 || length != 2000 {
		t.Errorf("TIFFPreview = (%d, %d, %v), want (100, 2000, true)", off, length, ok)
	}
}
