// Package turbo decodes JPEGs via libjpeg-turbo (~4-6x faster than Go's
// image/jpeg for the X-H2S's 26 MP files, and SIMD-parallel across cores
// when called from multiple goroutines).
package turbo

/*
#cgo LDFLAGS: -lturbojpeg
#include <stdlib.h>
#include <turbojpeg.h>
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"
)

// Image is a decoded RGBA frame (8 bits per channel, row-major).
type Image struct {
	Pix  []byte
	W, H int
}

// Decode decompresses a JPEG byte stream to RGBA using a per-call handle
// (handles are cheap; this keeps Decode goroutine-safe).
func Decode(data []byte) (*Image, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty jpeg data")
	}
	h := C.tjInitDecompress()
	if h == nil {
		return nil, fmt.Errorf("tjInitDecompress failed")
	}
	defer C.tjDestroy(h)

	var w, hgt, subsamp, colorspace C.int
	src := (*C.uchar)(unsafe.Pointer(&data[0]))
	if C.tjDecompressHeader3(h, src, C.ulong(len(data)), &w, &hgt, &subsamp, &colorspace) != 0 {
		return nil, fmt.Errorf("tjDecompressHeader3: %s", C.GoString(C.tjGetErrorStr2(h)))
	}
	img := &Image{Pix: make([]byte, int(w)*int(hgt)*4), W: int(w), H: int(hgt)}
	dst := (*C.uchar)(unsafe.Pointer(&img.Pix[0]))
	if C.tjDecompress2(h, src, C.ulong(len(data)), dst, w, w*4, hgt, C.TJPF_RGBA, C.TJFLAG_FASTDCT) != 0 {
		return nil, fmt.Errorf("tjDecompress2: %s", C.GoString(C.tjGetErrorStr2(h)))
	}
	return img.normalize(Orientation(data)), nil
}

// DecodeFile decodes a JPEG file from disk.
func DecodeFile(path string) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

/* ── EXIF orientation ─────────────────────────────────────── */

// Orientation extracts the EXIF orientation (1-8; 1 = upright) from a JPEG
// byte stream with a minimal APP1/TIFF scan — libjpeg-turbo ignores EXIF, so
// callers must normalize pixels themselves.
func Orientation(data []byte) int {
	// walk JPEG segments looking for APP1/Exif
	i := 2
	for i+4 < len(data) {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xDA { // start of scan: no EXIF past here
			return 1
		}
		seglen := int(data[i+2])<<8 | int(data[i+3])
		if marker == 0xE1 && i+4+6 < len(data) && string(data[i+4:i+10]) == "Exif\x00\x00" {
			return tiffOrientation(data[i+10 : i+2+seglen])
		}
		i += 2 + seglen
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 14 {
		return 1
	}
	var be bool
	switch string(t[0:2]) {
	case "MM":
		be = true
	case "II":
		be = false
	default:
		return 1
	}
	u16 := func(o int) int {
		if o+2 > len(t) {
			return 0
		}
		if be {
			return int(t[o])<<8 | int(t[o+1])
		}
		return int(t[o+1])<<8 | int(t[o])
	}
	u32 := func(o int) int {
		if o+4 > len(t) {
			return 0
		}
		if be {
			return int(t[o])<<24 | int(t[o+1])<<16 | int(t[o+2])<<8 | int(t[o+3])
		}
		return int(t[o+3])<<24 | int(t[o+2])<<16 | int(t[o+1])<<8 | int(t[o])
	}
	ifd := u32(4)
	n := u16(ifd)
	for e := 0; e < n; e++ {
		off := ifd + 2 + e*12
		if u16(off) == 0x0112 {
			v := u16(off + 8)
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// normalize rewrites the pixels upright according to EXIF orientation.
func (m *Image) normalize(orient int) *Image {
	switch orient {
	case 2:
		m.flipH()
	case 3:
		m.rotate180()
	case 4:
		m.rotate180()
		m.flipH()
	case 5:
		r := m.rotate90()
		r.flipH()
		return r
	case 6:
		return m.rotate90()
	case 7:
		r := m.rotate270()
		r.flipH()
		return r
	case 8:
		return m.rotate270()
	}
	return m
}

func (m *Image) rotate90() *Image { // clockwise
	out := &Image{Pix: make([]byte, len(m.Pix)), W: m.H, H: m.W}
	for y := 0; y < m.H; y++ {
		row := m.Pix[y*m.W*4:]
		for x := 0; x < m.W; x++ {
			dst := (x*out.W + (out.W - 1 - y)) * 4
			copy(out.Pix[dst:dst+4], row[x*4:x*4+4])
		}
	}
	return out
}

func (m *Image) rotate270() *Image { // counter-clockwise
	out := &Image{Pix: make([]byte, len(m.Pix)), W: m.H, H: m.W}
	for y := 0; y < m.H; y++ {
		row := m.Pix[y*m.W*4:]
		for x := 0; x < m.W; x++ {
			dst := ((out.H-1-x)*out.W + y) * 4
			copy(out.Pix[dst:dst+4], row[x*4:x*4+4])
		}
	}
	return out
}

func (m *Image) rotate180() {
	n := m.W * m.H
	for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
		a, b := m.Pix[i*4:i*4+4], m.Pix[j*4:j*4+4]
		for k := 0; k < 4; k++ {
			a[k], b[k] = b[k], a[k]
		}
	}
}

func (m *Image) flipH() {
	for y := 0; y < m.H; y++ {
		row := m.Pix[y*m.W*4 : (y+1)*m.W*4]
		for x, xr := 0, m.W-1; x < xr; x, xr = x+1, xr-1 {
			a, b := row[x*4:x*4+4], row[xr*4:xr*4+4]
			for k := 0; k < 4; k++ {
				a[k], b[k] = b[k], a[k]
			}
		}
	}
}
