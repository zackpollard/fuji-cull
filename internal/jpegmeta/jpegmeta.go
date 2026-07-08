// Package jpegmeta extracts metadata from JPEG byte streams with a minimal
// segment walk — no image decode, so it is cheap enough to run on every file
// that passes through the buffer and needs only the head of the file (EXIF
// APP1 sits directly after SOI; 64 KB is ample).
package jpegmeta

// Orientation extracts the EXIF orientation (1-8; 1 = upright) from a JPEG
// byte stream. Returns 1 when no orientation tag is present.
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
			end := i + 2 + seglen
			if end > len(data) {
				end = len(data)
			}
			return tiffOrientation(data[i+10 : end])
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
