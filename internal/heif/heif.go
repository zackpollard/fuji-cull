// Package heif locates the images embedded in a HEIF still without decoding
// it. A Fuji HEIF carries the same 160x120 thumbnail a JPEG does, plus larger
// JPEG renditions, but they live inside the ISO-BMFF container rather than in
// an EXIF segment — and the camera refuses GetThumb for these objects
// (NoThumbnailPresent), so the thumbnail has to come out of the file itself.
//
// The point of parsing rather than scanning is cost: the "meta" box is about a
// kilobyte at the front of the file and states exactly where every item lives,
// so a thumbnail costs a 4 KB read plus the ~11 KB of the thumbnail, instead of
// pulling megabytes of a file over MTP to find it.
package heif

import "encoding/binary"

// Item is one stored item: where it sits in the file, and what it is.
type Item struct {
	Type   string // "jpeg", "Exif", "hvc1", ...
	Offset int64
	Length int64
}

// IsHEIF reports whether data opens like an ISO-BMFF file. HEIF shares the
// container with MP4, so the caller still decides by extension what it has.
func IsHEIF(data []byte) bool {
	return len(data) >= 8 && string(data[4:8]) == "ftyp"
}

// Items returns the items described by the file's meta box. head need only
// span ftyp+meta — a few KB — even though the offsets it returns point deep
// into the file. ok is false when the head is too short or carries no meta box.
func Items(head []byte) (items []Item, ok bool) {
	meta, found := topLevelBox(head, "meta")
	if !found {
		return nil, false
	}
	if len(meta) < 4 {
		return nil, false
	}
	meta = meta[4:] // meta is a FullBox: skip version+flags

	types := map[uint32]string{}
	if iinf, found := topLevelBox(meta, "iinf"); found {
		parseIinf(iinf, types)
	}
	iloc, found := topLevelBox(meta, "iloc")
	if !found {
		return nil, false
	}
	locs := parseIloc(iloc)
	for id, l := range locs {
		l.Type = types[id]
		items = append(items, l)
	}
	return items, len(items) > 0
}

// Smallest returns the smallest item of the given type — the thumbnail, when
// asked for "jpeg", since a HEIF stores its renditions largest to smallest.
func Smallest(items []Item, typ string) (Item, bool) {
	var best Item
	found := false
	for _, it := range items {
		if it.Type != typ || it.Length <= 0 {
			continue
		}
		if !found || it.Length < best.Length {
			best, found = it, true
		}
	}
	return best, found
}

// Largest returns the largest item of the given type: the biggest JPEG a HEIF
// carries is a full preview rendition, big enough to display.
func Largest(items []Item, typ string) (Item, bool) {
	var best Item
	found := false
	for _, it := range items {
		if it.Type != typ || it.Length <= 0 {
			continue
		}
		if !found || it.Length > best.Length {
			best, found = it, true
		}
	}
	return best, found
}

// topLevelBox finds a box by type among data's siblings, returning its payload.
func topLevelBox(data []byte, want string) ([]byte, bool) {
	for off := 0; off+8 <= len(data); {
		size := int64(binary.BigEndian.Uint32(data[off:]))
		typ := string(data[off+4 : off+8])
		body := off + 8
		switch {
		case size == 1: // 64-bit size follows the type
			if body+8 > len(data) {
				return nil, false
			}
			size = int64(binary.BigEndian.Uint64(data[body:]))
			body += 8
		case size == 0: // extends to end of file
			size = int64(len(data) - off)
		}
		if size < 8 || off+int(size) > len(data) {
			// The box runs past what we read; a truncated head still lets us
			// return the boxes that fit entirely inside it.
			if typ == want && body <= len(data) {
				return data[body:], true
			}
			return nil, false
		}
		if typ == want {
			return data[body : off+int(size)], true
		}
		off += int(size)
	}
	return nil, false
}

// parseIinf maps item IDs to their four-character type.
func parseIinf(b []byte, out map[uint32]string) {
	if len(b) < 4 {
		return
	}
	version := b[0]
	off := 4
	if version == 0 {
		off += 2 // entry_count is 16-bit
	} else {
		off += 4
	}
	for off+8 <= len(b) {
		size := int64(binary.BigEndian.Uint32(b[off:]))
		if size < 8 || off+int(size) > len(b) {
			return
		}
		if string(b[off+4:off+8]) == "infe" {
			e := b[off+8 : off+int(size)]
			if len(e) >= 4 {
				v := e[0]
				p := 4
				var id uint32
				switch {
				case v >= 2 && v < 3 && p+2 <= len(e):
					id = uint32(binary.BigEndian.Uint16(e[p:]))
					p += 2
				case v >= 3 && p+4 <= len(e):
					id = binary.BigEndian.Uint32(e[p:])
					p += 4
				default: // v0/v1 carry no item_type at all
					p = len(e)
				}
				if p+6 <= len(e) {
					p += 2 // protection_index
					out[id] = string(e[p : p+4])
				}
			}
		}
		off += int(size)
	}
}

// parseIloc reads the item location box: per item, a base offset plus extents.
func parseIloc(b []byte) map[uint32]Item {
	out := map[uint32]Item{}
	if len(b) < 8 {
		return out
	}
	version := b[0]
	p := 4
	offsetSize := int(b[p] >> 4)
	lengthSize := int(b[p] & 0x0F)
	baseOffsetSize := int(b[p+1] >> 4)
	indexSize := 0
	if version == 1 || version == 2 {
		indexSize = int(b[p+1] & 0x0F)
	}
	p += 2

	var count int
	if version < 2 {
		if p+2 > len(b) {
			return out
		}
		count = int(binary.BigEndian.Uint16(b[p:]))
		p += 2
	} else {
		if p+4 > len(b) {
			return out
		}
		count = int(binary.BigEndian.Uint32(b[p:]))
		p += 4
	}

	num := func(width int) (int64, bool) {
		if width == 0 {
			return 0, true
		}
		if p+width > len(b) {
			return 0, false
		}
		var v int64
		for i := 0; i < width; i++ {
			v = v<<8 | int64(b[p+i])
		}
		p += width
		return v, true
	}

	for i := 0; i < count; i++ {
		var id uint32
		if version < 2 {
			if p+2 > len(b) {
				return out
			}
			id = uint32(binary.BigEndian.Uint16(b[p:]))
			p += 2
		} else {
			if p+4 > len(b) {
				return out
			}
			id = binary.BigEndian.Uint32(b[p:])
			p += 4
		}
		if version == 1 || version == 2 {
			p += 2 // construction_method
		}
		p += 2 // data_reference_index
		base, ok := num(baseOffsetSize)
		if !ok {
			return out
		}
		if p+2 > len(b) {
			return out
		}
		extents := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		// One item, one contiguous rendition: the first extent is the whole
		// of it for every HEIF a camera writes.
		for e := 0; e < extents; e++ {
			if _, ok := num(indexSize); !ok {
				return out
			}
			off, ok := num(offsetSize)
			if !ok {
				return out
			}
			ln, ok := num(lengthSize)
			if !ok {
				return out
			}
			if e == 0 {
				out[id] = Item{Offset: base + off, Length: ln}
			}
		}
	}
	return out
}

// ExifTIFF returns the TIFF payload of an "Exif" item. The item begins with a
// 4-byte offset to the TIFF header (almost always zero), after which it is an
// ordinary EXIF block — the same bytes a JPEG carries in its APP1 segment.
func ExifTIFF(item []byte) []byte {
	if len(item) < 4 {
		return nil
	}
	skip := int(binary.BigEndian.Uint32(item[:4]))
	if 4+skip > len(item) {
		return nil
	}
	return item[4+skip:]
}
