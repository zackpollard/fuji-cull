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

	"github.com/zack/fuji-tools/internal/jpegmeta"
)

// Image is a decoded RGBA frame (8 bits per channel, row-major).
type Image struct {
	Pix  []byte
	W, H int
	// NatW/NatH are the frame's size in the file, after the orientation fix
	// and regardless of the scale it was decoded at. Geometry — how big to
	// draw it, what counts as 100% zoom — is a property of the photograph,
	// not of how many pixels we chose to decode, so callers size from these
	// and let the renderer stretch the texture.
	NatW, NatH int
}

// Decode decompresses a JPEG byte stream to RGBA at the file's own size.
func Decode(data []byte) (*Image, error) { return DecodeScaled(data, 0, 0) }

// DecodeScaled decompresses a JPEG to RGBA big enough to fill a boxW x boxH
// letterbox and no bigger,
// letting libjpeg-turbo drop the surplus resolution inside the IDCT rather
// than decoding pixels the screen will never show. A 40 MP frame shown on a
// 4K display needs a quarter of its pixels; decoding it whole costs that
// factor four times over — in the decode, in the 151 MB RGBA buffer, in the
// texture upload, and again in every per-pixel pass (focus peaking) run over
// it. Scaling happens before the orientation fix, so maxW/maxH are in
// display space and get swapped here for a rotated frame.
//
// A box of 0 means "the file's own size": callers that genuinely need every
// pixel — a zoom past what the fitted rendition holds — ask for it.
func DecodeScaled(data []byte, boxW, boxH int) (*Image, error) {
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

	orient := jpegmeta.Orientation(data)
	reqW, reqH := boxW, boxH
	if orient >= 5 && orient <= 8 { // the fix below transposes the frame
		reqW, reqH = reqH, reqW
	}
	dw, dh := scaledSize(int(w), int(hgt), reqW, reqH)

	img := &Image{Pix: make([]byte, dw*dh*4), W: dw, H: dh}
	dst := (*C.uchar)(unsafe.Pointer(&img.Pix[0]))
	if C.tjDecompress2(h, src, C.ulong(len(data)), dst, C.int(dw), C.int(dw*4), C.int(dh), C.TJPF_RGBA, C.TJFLAG_FASTDCT) != 0 {
		return nil, fmt.Errorf("tjDecompress2: %s", C.GoString(C.tjGetErrorStr2(h)))
	}
	out := img.Normalize(orient)
	out.NatW, out.NatH = int(w), int(hgt)
	if orient >= 5 && orient <= 8 {
		out.NatW, out.NatH = int(hgt), int(w)
	}
	return out, nil
}

// scaledSize picks the smallest libjpeg-turbo scaling factor whose output
// still has every pixel the screen can show. The frame is letterboxed into
// boxW x boxH, so what matters is the fitted size, not the box: a tall frame
// on a wide screen is limited by its height and the width the box offers is
// never used. Only the factors the library advertises are considered — those
// are the ones the scaled IDCT implements, so they cost less than a full
// decode rather than more. Falls back to the native size when nothing fits,
// including when the caller passes 0 meaning "no limit".
func scaledSize(w, hgt, boxW, boxH int) (int, int) {
	if boxW <= 0 || boxH <= 0 || w <= 0 || hgt <= 0 {
		return w, hgt
	}
	fit := float64(boxW) / float64(w)
	if f := float64(boxH) / float64(hgt); f < fit {
		fit = f
	}
	if fit >= 1 { // the box wants more pixels than the file holds
		return w, hgt
	}
	needW := int(float64(w)*fit) + 1
	needH := int(float64(hgt)*fit) + 1

	var n C.int
	fs := C.tjGetScalingFactors(&n)
	if fs == nil || n <= 0 {
		return w, hgt
	}
	bw, bh := w, hgt
	for _, f := range unsafe.Slice(fs, int(n)) {
		num, den := int(f.num), int(f.denom)
		if num > den { // upscaling: more pixels than the file has
			continue
		}
		sw := (w*num + den - 1) / den
		sh := (hgt*num + den - 1) / den
		if sw >= needW && sh >= needH && sw*sh < bw*bh {
			bw, bh = sw, sh
		}
	}
	return bw, bh
}

// DecodeFile decodes a JPEG file from disk at its own size.
func DecodeFile(path string) (*Image, error) { return DecodeScaledFile(path, 0, 0) }

// DecodeScaledFile decodes a JPEG file from disk, sized for a boxW x boxH fit.
func DecodeScaledFile(path string, boxW, boxH int) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeScaled(data, boxW, boxH)
}

/* ── EXIF orientation ─────────────────────────────────────── */

// Normalize rewrites the pixels upright according to an EXIF orientation
// (1-8) — libjpeg-turbo ignores EXIF, and thumbnails carry no EXIF at all, so
// callers with an out-of-band orientation apply it here.
func (m *Image) Normalize(orient int) *Image {
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
