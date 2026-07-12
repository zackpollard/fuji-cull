// Package jpegmeta extracts metadata from JPEG byte streams with a minimal
// segment walk — no image decode, so it is cheap enough to run on every file
// that passes through the buffer and needs only the head of the file (EXIF
// APP1 sits directly after SOI; 128 KB is ample).
package jpegmeta

// jpegStream returns the JPEG byte stream inside data: data itself for JPEG
// files, or the embedded preview JPEG for Fuji RAF files (whose header
// stores the preview's offset big-endian at byte 84 — the preview and its
// EXIF sit comfortably inside a 128 KB head read).
func jpegStream(data []byte) []byte {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8 {
		return data
	}
	if len(data) < 92 || string(data[:8]) != "FUJIFILM" {
		return nil
	}
	off := int(data[84])<<24 | int(data[85])<<16 | int(data[86])<<8 | int(data[87])
	if off <= 0 || off+2 > len(data) || data[off] != 0xFF || data[off+1] != 0xD8 {
		return nil
	}
	return data[off:]
}

// app1 returns the TIFF payload of the JPEG's APP1/Exif segment, or nil.
func app1(data []byte) []byte {
	i := 2
	for i+4 < len(data) {
		if data[i] != 0xFF {
			return nil
		}
		marker := data[i+1]
		if marker == 0xDA { // start of scan: no EXIF past here
			return nil
		}
		seglen := int(data[i+2])<<8 | int(data[i+3])
		if marker == 0xE1 && i+4+6 < len(data) && string(data[i+4:i+10]) == "Exif\x00\x00" {
			end := i + 2 + seglen
			if end > len(data) {
				end = len(data)
			}
			return data[i+10 : end]
		}
		i += 2 + seglen
	}
	return nil
}

// tiff is a bounds-checked reader over an APP1 TIFF payload.
type tiff struct {
	d  []byte
	be bool
}

func newTIFF(t []byte) (tiff, bool) {
	if len(t) < 14 {
		return tiff{}, false
	}
	switch string(t[0:2]) {
	case "MM":
		return tiff{d: t, be: true}, true
	case "II":
		return tiff{d: t, be: false}, true
	}
	return tiff{}, false
}

func (t tiff) u16(o int) int {
	if o < 0 || o+2 > len(t.d) {
		return 0
	}
	if t.be {
		return int(t.d[o])<<8 | int(t.d[o+1])
	}
	return int(t.d[o+1])<<8 | int(t.d[o])
}

func (t tiff) u32(o int) int {
	if o < 0 || o+4 > len(t.d) {
		return 0
	}
	if t.be {
		return int(t.d[o])<<24 | int(t.d[o+1])<<16 | int(t.d[o+2])<<8 | int(t.d[o+3])
	}
	return int(t.d[o+3])<<24 | int(t.d[o+2])<<16 | int(t.d[o+1])<<8 | int(t.d[o])
}

// findTag scans one IFD for a tag, returning the offset of its value field.
func (t tiff) findTag(ifd, tag int) int {
	n := t.u16(ifd)
	for e := 0; e < n; e++ {
		off := ifd + 2 + e*12
		if t.u16(off) == tag {
			return off + 8
		}
	}
	return -1
}

// Orientation extracts the EXIF orientation (1-8; 1 = upright) from a JPEG
// or Fuji RAF byte stream. Returns 1 when no orientation tag is present.
func Orientation(data []byte) int {
	t, ok := newTIFF(app1(jpegStream(data)))
	if !ok {
		return 1
	}
	if v := t.findTag(t.u32(4), 0x0112); v >= 0 {
		if o := t.u16(v); o >= 1 && o <= 8 {
			return o
		}
	}
	return 1
}

// DateTimeOriginal extracts the EXIF capture time ("2006:01:02 15:04:05")
// from a JPEG or Fuji RAF byte stream, falling back to IFD0's ModifyDate.
// Returns "" when absent.
func DateTimeOriginal(data []byte) string {
	t, ok := newTIFF(app1(jpegStream(data)))
	if !ok {
		return ""
	}
	ifd0 := t.u32(4)
	// DateTimeOriginal (0x9003) lives in the Exif sub-IFD (pointer 0x8769)
	if v := t.findTag(ifd0, 0x8769); v >= 0 {
		if s := t.ascii(t.findTag(t.u32(v), 0x9003)); s != "" {
			return s
		}
	}
	return t.ascii(t.findTag(ifd0, 0x0132))
}

// ascii reads an EXIF ASCII value: the value field holds an offset into the
// TIFF payload for strings longer than 4 bytes (dates always are).
func (t tiff) ascii(valueOff int) string {
	if valueOff < 0 {
		return ""
	}
	off := t.u32(valueOff)
	end := off
	for end < len(t.d) && t.d[end] != 0 && end-off < 32 {
		end++
	}
	if end <= off {
		return ""
	}
	return string(t.d[off:end])
}

// Thumbnail extracts the EXIF-embedded thumbnail JPEG (IFD1's
// JPEGInterchangeFormat), or nil when absent or truncated. Fuji cameras
// embed the same 160×120 preview that MTP GetThumb serves, so a file head
// can substitute for a thumbnail transfer entirely.
func Thumbnail(data []byte) []byte {
	t, ok := newTIFF(app1(jpegStream(data)))
	if !ok {
		return nil
	}
	ifd0 := t.u32(4)
	n := t.u16(ifd0)
	ifd1 := t.u32(ifd0 + 2 + n*12) // next-IFD pointer after IFD0's entries
	if ifd1 <= 0 {
		return nil
	}
	offAt := t.findTag(ifd1, 0x0201) // JPEGInterchangeFormat
	lenAt := t.findTag(ifd1, 0x0202) // JPEGInterchangeFormatLength
	if offAt < 0 || lenAt < 0 {
		return nil
	}
	off, ln := t.u32(offAt), t.u32(lenAt)
	if off <= 0 || ln < 4 || off+ln > len(t.d) {
		return nil
	}
	th := t.d[off : off+ln]
	if th[0] != 0xFF || th[1] != 0xD8 || th[ln-2] != 0xFF || th[ln-1] != 0xD9 {
		return nil
	}
	return th
}
