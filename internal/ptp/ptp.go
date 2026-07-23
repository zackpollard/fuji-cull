// Package ptp encodes the handful of PTP/MTP requests the engine needs and
// parses their datasets.
//
// On iOS, ImageCaptureCore's requestSendPTPCommand provides the raw pipe for
// these — but only behind three undocumented gates (NSCameraUsageDescription,
// a control-authorization grant, and ICC's content catalog completing; miss
// any and commands are dropped silently, with no callback and no error).
// Since the mandatory catalog is itself a full index and requestReadData
// covers partial reads, the shipping iOS path is object-level
// (cull.Transport); this package is the protocol layer for card-wide
// GetObjectPropList sweeps over the passthrough when they're needed (e.g.
// bulk capture dates), verified answering on the X-H2S post-catalog.
//
// The requests mirror what the aft patch issues, notably the card-wide
// GetObjectPropList sweep ("lsprops-all"): a few bulk round-trips instead of a
// per-object info storm, which is the difference between seconds and never
// finishing on a 19k-file card.
package ptp

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// Operation codes.
const (
	OpGetDeviceInfo     uint16 = 0x1001
	OpGetStorageIDs     uint16 = 0x1004
	OpGetObjectHandles  uint16 = 0x1007
	OpGetObjectInfo     uint16 = 0x1008
	OpGetPartialObject  uint16 = 0x101B
	OpGetObjectPropList uint16 = 0x9805
)

// Object property codes.
const (
	PropStorageID    uint16 = 0xDC01
	PropObjectFormat uint16 = 0xDC02
	PropObjectSize   uint16 = 0xDC04
	PropFileName     uint16 = 0xDC07
	PropDateCreated  uint16 = 0xDC08
	PropParentObject uint16 = 0xDC0B
)

// HandleAll addresses every object on the device — the only GetObjectPropList
// shape the X-H2S honors card-wide (with depth 0).
const HandleAll uint32 = 0xFFFFFFFF

// Command builds a PTP command container: length, type=1 (command), opcode,
// transaction id, then parameters.
func Command(op uint16, txn uint32, params ...uint32) []byte {
	b := make([]byte, 12+4*len(params))
	binary.LittleEndian.PutUint32(b[0:], uint32(len(b)))
	binary.LittleEndian.PutUint16(b[4:], 1)
	binary.LittleEndian.PutUint16(b[6:], op)
	binary.LittleEndian.PutUint32(b[8:], txn)
	for i, p := range params {
		binary.LittleEndian.PutUint32(b[12+4*i:], p)
	}
	return b
}

// PropListAll builds the card-wide GetObjectPropList request for one property.
func PropListAll(prop uint16, txn uint32) []byte {
	// handle, objectFormat(any), property, groupCode, depth
	return Command(OpGetObjectPropList, txn, HandleAll, 0, uint32(prop), 0, 0)
}

// PartialObject builds a GetPartialObject request. Offset and size are 32-bit
// in this operation — objects past 4 GiB need GetPartialObject64, which the
// X-H2S does not implement (same ceiling as the desktop build).
func PartialObject(handle uint32, offset, size uint32, txn uint32) []byte {
	return Command(OpGetPartialObject, txn, handle, offset, size)
}

// PropEntry is one (object, property, value) triple from an ObjectPropList.
type PropEntry struct {
	Handle uint32
	Prop   uint16
	Num    uint64
	Str    string
	IsStr  bool
}

// ParsePropList decodes an ObjectPropList dataset:
//
//	u32 count, then per element: u32 handle, u16 property, u16 datatype, value
//
// A leading data-phase container header is skipped when present, since hosts
// differ in whether they strip it.
func ParsePropList(b []byte) ([]PropEntry, error) {
	r := &reader{b: b}
	// data-phase container header: length, type==2, code, transaction
	if len(b) >= 12 {
		if l := binary.LittleEndian.Uint32(b); int(l) <= len(b) && l >= 12 &&
			binary.LittleEndian.Uint16(b[4:]) == 2 {
			r.i = 12
		}
	}
	count, ok := r.u32()
	if !ok {
		return nil, fmt.Errorf("propList: truncated header")
	}
	if count > 5_000_000 {
		return nil, fmt.Errorf("propList: implausible element count %d", count)
	}
	out := make([]PropEntry, 0, count)
	for n := uint32(0); n < count; n++ {
		handle, ok1 := r.u32()
		prop, ok2 := r.u16()
		typ, ok3 := r.u16()
		if !ok1 || !ok2 || !ok3 {
			return out, fmt.Errorf("propList: truncated at element %d/%d", n, count)
		}
		e := PropEntry{Handle: handle, Prop: prop}
		switch typ {
		case 0x0001, 0x0002: // INT8 / UINT8
			v, ok := r.u8()
			if !ok {
				return out, fmt.Errorf("propList: truncated value")
			}
			e.Num = uint64(v)
		case 0x0003, 0x0004: // INT16 / UINT16
			v, ok := r.u16()
			if !ok {
				return out, fmt.Errorf("propList: truncated value")
			}
			e.Num = uint64(v)
		case 0x0005, 0x0006: // INT32 / UINT32
			v, ok := r.u32()
			if !ok {
				return out, fmt.Errorf("propList: truncated value")
			}
			e.Num = uint64(v)
		case 0x0007, 0x0008: // INT64 / UINT64
			v, ok := r.u64()
			if !ok {
				return out, fmt.Errorf("propList: truncated value")
			}
			e.Num = v
		case 0x0009, 0x000A: // INT128 / UINT128 — low word is enough here
			lo, ok1 := r.u64()
			_, ok2 := r.u64()
			if !ok1 || !ok2 {
				return out, fmt.Errorf("propList: truncated value")
			}
			e.Num = lo
		case 0xFFFF: // MTP string
			s, ok := r.str()
			if !ok {
				return out, fmt.Errorf("propList: truncated string")
			}
			e.Str, e.IsStr = s, true
		default:
			return out, fmt.Errorf("propList: unsupported datatype 0x%04x", typ)
		}
		out = append(out, e)
	}
	return out, nil
}

// CaptureDay converts an MTP datetime ("YYYYMMDDThhmmss") to "YYYY-MM-DD",
// matching what the catalog groups the timeline by. Returns "" when unparseable.
func CaptureDay(s string) string {
	if len(s) < 8 {
		return ""
	}
	for i := 0; i < 8; i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
	}
	return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
}

type reader struct {
	b []byte
	i int
}

func (r *reader) u8() (uint8, bool) {
	if r.i+1 > len(r.b) {
		return 0, false
	}
	v := r.b[r.i]
	r.i++
	return v, true
}

func (r *reader) u16() (uint16, bool) {
	if r.i+2 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint16(r.b[r.i:])
	r.i += 2
	return v, true
}

func (r *reader) u32() (uint32, bool) {
	if r.i+4 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint32(r.b[r.i:])
	r.i += 4
	return v, true
}

func (r *reader) u64() (uint64, bool) {
	if r.i+8 > len(r.b) {
		return 0, false
	}
	v := binary.LittleEndian.Uint64(r.b[r.i:])
	r.i += 8
	return v, true
}

// str reads an MTP string: u8 length in UTF-16 units (including the trailing
// NUL), then that many UTF-16LE units.
func (r *reader) str() (string, bool) {
	n, ok := r.u8()
	if !ok {
		return "", false
	}
	if n == 0 {
		return "", true
	}
	if r.i+int(n)*2 > len(r.b) {
		return "", false
	}
	units := make([]uint16, 0, n)
	for k := 0; k < int(n); k++ {
		u := binary.LittleEndian.Uint16(r.b[r.i+k*2:])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	r.i += int(n) * 2
	return string(utf16.Decode(units)), true
}
