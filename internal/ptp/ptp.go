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

var opNames = map[uint16]string{
	OpGetDeviceInfo:     "GetDeviceInfo",
	OpGetStorageIDs:     "GetStorageIDs",
	OpGetObjectHandles:  "GetObjectHandles",
	OpGetObjectInfo:     "GetObjectInfo",
	OpGetPartialObject:  "GetPartialObject",
	OpGetObjectPropList: "GetObjectPropList",
}

// OpName names an operation code for a log line. A diagnostic that does not
// say WHICH command was refused only halves the mystery.
func OpName(op uint16) string {
	if n, ok := opNames[op]; ok {
		return n
	}
	return fmt.Sprintf("op(0x%04x)", op)
}

// CommandOp reads the opcode back out of a command container, so a caller
// holding only the bytes can still name what it sent.
func CommandOp(b []byte) (uint16, bool) {
	if len(b) < 8 || binary.LittleEndian.Uint16(b[4:]) != 1 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b[6:]), true
}

// Response codes. The subset a Fuji body actually answers with, plus the
// generic ones any PTP device can return.
//
// These exist because the response container was being thrown away: a camera
// that refuses a command answers with a code saying WHY, and without it every
// refusal looked identical to a timeout or an empty data phase. "PTP command
// returned no data" cost a debugging session that a two-byte code would have
// ended immediately.
const (
	RespOK                      uint16 = 0x2001
	RespGeneralError            uint16 = 0x2002
	RespSessionNotOpen          uint16 = 0x2003
	RespInvalidTransactionID    uint16 = 0x2004
	RespOperationNotSupported   uint16 = 0x2005
	RespParameterNotSupported   uint16 = 0x2006
	RespIncompleteTransfer      uint16 = 0x2007
	RespInvalidStorageID        uint16 = 0x2008
	RespInvalidObjectHandle     uint16 = 0x2009
	RespDevicePropNotSupported  uint16 = 0x200A
	RespStoreFull               uint16 = 0x200C
	RespStoreNotAvailable       uint16 = 0x2013
	RespSpecByFormatUnsupported uint16 = 0x2014
	RespNoValidObjectInfo       uint16 = 0x2015
	RespDeviceBusy              uint16 = 0x2019
	RespInvalidParentObject     uint16 = 0x201A
	RespInvalidParameter        uint16 = 0x201D
	RespSessionAlreadyOpen      uint16 = 0x201E
	RespTransactionCancelled    uint16 = 0x201F
	RespInvalidObjectPropCode   uint16 = 0xA801
	RespInvalidObjectPropFormat uint16 = 0xA802
	RespInvalidObjectPropValue  uint16 = 0xA803
	RespObjectPropNotSupported  uint16 = 0xA80A
)

var respNames = map[uint16]string{
	RespOK:                      "OK",
	RespGeneralError:            "GeneralError",
	RespSessionNotOpen:          "SessionNotOpen",
	RespInvalidTransactionID:    "InvalidTransactionID",
	RespOperationNotSupported:   "OperationNotSupported",
	RespParameterNotSupported:   "ParameterNotSupported",
	RespIncompleteTransfer:      "IncompleteTransfer",
	RespInvalidStorageID:        "InvalidStorageID",
	RespInvalidObjectHandle:     "InvalidObjectHandle",
	RespDevicePropNotSupported:  "DevicePropNotSupported",
	RespStoreFull:               "StoreFull",
	RespStoreNotAvailable:       "StoreNotAvailable",
	RespSpecByFormatUnsupported: "SpecificationByFormatUnsupported",
	RespNoValidObjectInfo:       "NoValidObjectInfo",
	RespDeviceBusy:              "DeviceBusy",
	RespInvalidParentObject:     "InvalidParentObject",
	RespInvalidParameter:        "InvalidParameter",
	RespSessionAlreadyOpen:      "SessionAlreadyOpen",
	RespTransactionCancelled:    "TransactionCancelled",
	RespInvalidObjectPropCode:   "InvalidObjectPropCode",
	RespInvalidObjectPropFormat: "InvalidObjectPropFormat",
	RespInvalidObjectPropValue:  "InvalidObjectPropValue",
	RespObjectPropNotSupported:  "ObjectPropNotSupported",
}

// Response is a decoded PTP response container.
type Response struct {
	Code   uint16
	Txn    uint32
	Params []uint32
}

// OK reports whether the device accepted the command.
func (r Response) OK() bool { return r.Code == RespOK }

// String renders a response for a log line: the name when known, the raw code
// either way — an unnamed code is still the most useful fact available.
func (r Response) String() string {
	if n, ok := respNames[r.Code]; ok {
		return fmt.Sprintf("%s (0x%04x)", n, r.Code)
	}
	return fmt.Sprintf("unknown (0x%04x)", r.Code)
}

// ResponseName is the name of a response code, or "" when it is not one we
// have a name for.
func ResponseName(code uint16) string { return respNames[code] }

// ParseResponse decodes a response container: length, type==3, code,
// transaction id, then up to five u32 parameters.
//
// Hosts differ in what they hand back — some pass the whole container, some
// only the payload — so a bare 2-byte code is accepted too rather than
// discarding the one diagnostic we came for.
func ParseResponse(b []byte) (Response, error) {
	var r Response
	if len(b) == 2 {
		r.Code = binary.LittleEndian.Uint16(b)
		return r, nil
	}
	if len(b) < 12 {
		return r, fmt.Errorf("response: %d bytes, want at least 12", len(b))
	}
	if typ := binary.LittleEndian.Uint16(b[4:]); typ != 3 {
		return r, fmt.Errorf("response: container type %d, want 3", typ)
	}
	r.Code = binary.LittleEndian.Uint16(b[6:])
	r.Txn = binary.LittleEndian.Uint32(b[8:])
	n := int(binary.LittleEndian.Uint32(b))
	if n > len(b) {
		n = len(b) // a truncated container still has a usable code
	}
	for off := 12; off+4 <= n && len(r.Params) < 5; off += 4 {
		r.Params = append(r.Params, binary.LittleEndian.Uint32(b[off:]))
	}
	return r, nil
}

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

// DeviceInfo is the identity subset of the PTP DeviceInfo dataset.
type DeviceInfo struct {
	Manufacturer string
	Model        string
	Serial       string
}

// ParseDeviceInfo walks a DeviceInfo dataset (with or without a leading
// data-phase container header) to its trailing identity strings.
func ParseDeviceInfo(b []byte) (DeviceInfo, error) {
	r := &reader{b: b}
	if len(b) >= 12 {
		if l := binary.LittleEndian.Uint32(b); int(l) <= len(b) && l >= 12 &&
			binary.LittleEndian.Uint16(b[4:]) == 2 {
			r.i = 12
		}
	}
	var di DeviceInfo
	fail := func() (DeviceInfo, error) { return di, fmt.Errorf("deviceInfo: truncated") }

	// StandardVersion u16, VendorExtensionID u32, VendorExtensionVersion u16
	if _, ok := r.u16(); !ok {
		return fail()
	}
	if _, ok := r.u32(); !ok {
		return fail()
	}
	if _, ok := r.u16(); !ok {
		return fail()
	}
	if _, ok := r.str(); !ok { // VendorExtensionDesc
		return fail()
	}
	if _, ok := r.u16(); !ok { // FunctionalMode
		return fail()
	}
	// four u16 arrays: operations, events, device props, capture formats —
	// plus image formats
	for k := 0; k < 5; k++ {
		n, ok := r.u32()
		if !ok || int(n)*2 > len(r.b)-r.i {
			return fail()
		}
		r.i += int(n) * 2
	}
	var ok bool
	if di.Manufacturer, ok = r.str(); !ok {
		return fail()
	}
	if di.Model, ok = r.str(); !ok {
		return fail()
	}
	if _, ok = r.str(); !ok { // DeviceVersion
		return fail()
	}
	if di.Serial, ok = r.str(); !ok {
		return fail()
	}
	return di, nil
}
