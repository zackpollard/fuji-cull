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
	return img, nil
}

// DecodeFile decodes a JPEG file from disk.
func DecodeFile(path string) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}
