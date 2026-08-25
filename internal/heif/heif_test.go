package heif

import (
	"encoding/binary"
	"testing"
)

// build a minimal HEIF: ftyp + meta{iinf{infe...} iloc}
func build(items []struct {
	id      uint16
	typ     string
	off, ln uint32
}) []byte {
	box := func(typ string, payload []byte) []byte {
		b := make([]byte, 8+len(payload))
		binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
		copy(b[4:], typ)
		copy(b[8:], payload)
		return b
	}
	var infes []byte
	for _, it := range items {
		p := []byte{2, 0, 0, 0} // version 2 FullBox
		p = binary.BigEndian.AppendUint16(p, it.id)
		p = binary.BigEndian.AppendUint16(p, 0) // protection
		p = append(p, []byte(it.typ)...)
		p = append(p, 0) // name
		infes = append(infes, box("infe", p)...)
	}
	iinf := append([]byte{0, 0, 0, 0}, byte(len(items)>>8), byte(len(items)))
	iinf = append(iinf, infes...)

	// iloc version 1: offset_size=4, length_size=4, base_offset_size=0, index_size=0
	il := []byte{1, 0, 0, 0, 0x44, 0x00}
	il = binary.BigEndian.AppendUint16(il, uint16(len(items)))
	for _, it := range items {
		il = binary.BigEndian.AppendUint16(il, it.id)
		il = binary.BigEndian.AppendUint16(il, 0) // construction_method
		il = binary.BigEndian.AppendUint16(il, 0) // data_reference_index
		il = binary.BigEndian.AppendUint16(il, 1) // extent_count
		il = binary.BigEndian.AppendUint32(il, it.off)
		il = binary.BigEndian.AppendUint32(il, it.ln)
	}
	meta := append([]byte{0, 0, 0, 0}, box("iinf", iinf)...)
	meta = append(meta, box("iloc", il)...)
	return append(box("ftyp", []byte("heic0000heic")), box("meta", meta)...)
}

func TestItemsLocatesRenditions(t *testing.T) {
	// the shape a Fuji HIF has: renditions largest first, thumbnail last
	head := build([]struct {
		id      uint16
		typ     string
		off, ln uint32
	}{
		{1, "jpeg", 20992, 583620},
		{2, "Exif", 4096, 4168},
		{3, "jpeg", 604672, 49466},
		{4, "jpeg", 654336, 11545},
		{5, "hvc1", 666112, 133093},
	})
	items, ok := Items(head)
	if !ok {
		t.Fatal("no items parsed")
	}
	if len(items) != 5 {
		t.Fatalf("parsed %d items, want 5", len(items))
	}
	th, ok := Smallest(items, "jpeg")
	if !ok || th.Offset != 654336 || th.Length != 11545 {
		t.Errorf("thumbnail = %+v, want offset 654336 length 11545", th)
	}
	big, ok := Largest(items, "jpeg")
	if !ok || big.Offset != 20992 {
		t.Errorf("largest jpeg = %+v, want offset 20992", big)
	}
	ex, ok := Smallest(items, "Exif")
	if !ok || ex.Offset != 4096 {
		t.Errorf("exif = %+v, want offset 4096", ex)
	}
	// the point of parsing: offsets far past the head we read
	if th.Offset < int64(len(head)) {
		t.Error("fixture too small to prove offsets exceed the head")
	}
}

func TestItemsRejectsNonHEIF(t *testing.T) {
	for name, b := range map[string][]byte{
		"jpeg":  {0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0},
		"empty": {},
		"short": {0, 0, 0, 8},
	} {
		if _, ok := Items(b); ok {
			t.Errorf("Items(%s) ok = true, want false", name)
		}
	}
}

func TestExifTIFF(t *testing.T) {
	item := append([]byte{0, 0, 0, 0}, []byte("II*\x00")...)
	if got := ExifTIFF(item); string(got) != "II*\x00" {
		t.Errorf("ExifTIFF = %q, want the TIFF header", got)
	}
	if ExifTIFF([]byte{0, 0}) != nil {
		t.Error("ExifTIFF of a truncated item should be nil")
	}
}
