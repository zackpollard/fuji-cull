package ptp

import (
	"encoding/binary"
	"testing"
)

func TestCommand(t *testing.T) {
	b := Command(OpGetPartialObject, 7, 0x1234, 0, 4096)
	if got := binary.LittleEndian.Uint32(b); int(got) != len(b) {
		t.Fatalf("length field %d, buffer %d", got, len(b))
	}
	if binary.LittleEndian.Uint16(b[4:]) != 1 {
		t.Fatalf("container type should be 1 (command)")
	}
	if binary.LittleEndian.Uint16(b[6:]) != OpGetPartialObject {
		t.Fatalf("wrong opcode")
	}
	if binary.LittleEndian.Uint32(b[8:]) != 7 {
		t.Fatalf("wrong transaction id")
	}
	if binary.LittleEndian.Uint32(b[12:]) != 0x1234 {
		t.Fatalf("wrong first param")
	}
	if len(b) != 12+3*4 {
		t.Fatalf("unexpected length %d", len(b))
	}
}

// mtpString encodes a Go string the way a device would.
func mtpString(s string) []byte {
	units := []rune(s)
	out := []byte{byte(len(units) + 1)}
	for _, r := range units {
		var u [2]byte
		binary.LittleEndian.PutUint16(u[:], uint16(r))
		out = append(out, u[:]...)
	}
	return append(out, 0, 0) // trailing NUL
}

func propList(entries ...[]byte) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(len(entries)))
	for _, e := range entries {
		b = append(b, e...)
	}
	return b
}

func numEntry(handle uint32, prop uint16, typ uint16, val uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, handle)
	binary.LittleEndian.PutUint16(b[4:], prop)
	binary.LittleEndian.PutUint16(b[6:], typ)
	switch typ {
	case 0x0006:
		v := make([]byte, 4)
		binary.LittleEndian.PutUint32(v, uint32(val))
		return append(b, v...)
	case 0x0008:
		v := make([]byte, 8)
		binary.LittleEndian.PutUint64(v, val)
		return append(b, v...)
	}
	panic("unsupported")
}

func strEntry(handle uint32, prop uint16, s string) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, handle)
	binary.LittleEndian.PutUint16(b[4:], prop)
	binary.LittleEndian.PutUint16(b[6:], 0xFFFF)
	return append(b, mtpString(s)...)
}

func TestParsePropListMixed(t *testing.T) {
	data := propList(
		strEntry(1000, PropFileName, "DSCF0001.JPG"),
		numEntry(1000, PropObjectSize, 0x0008, 14379874),
		numEntry(1000, PropParentObject, 0x0006, 42),
		strEntry(1001, PropFileName, "DSCF0002.RAF"),
	)
	got, err := ParsePropList(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d", len(got))
	}
	if !got[0].IsStr || got[0].Str != "DSCF0001.JPG" || got[0].Handle != 1000 {
		t.Fatalf("bad filename entry: %+v", got[0])
	}
	if got[1].Num != 14379874 {
		t.Fatalf("bad size: %+v", got[1])
	}
	if got[2].Num != 42 {
		t.Fatalf("bad parent: %+v", got[2])
	}
	if got[3].Str != "DSCF0002.RAF" {
		t.Fatalf("bad second filename: %+v", got[3])
	}
}

// Some hosts hand back the data-phase container header; it must be skipped.
func TestParsePropListWithContainerHeader(t *testing.T) {
	body := propList(strEntry(7, PropFileName, "A.JPG"))
	hdr := make([]byte, 12)
	binary.LittleEndian.PutUint32(hdr, uint32(12+len(body)))
	binary.LittleEndian.PutUint16(hdr[4:], 2) // data container
	binary.LittleEndian.PutUint16(hdr[6:], OpGetObjectPropList)
	got, err := ParsePropList(append(hdr, body...))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Str != "A.JPG" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParsePropListRejectsGarbage(t *testing.T) {
	if _, err := ParsePropList([]byte{1, 2}); err == nil {
		t.Fatal("expected error on truncated input")
	}
}

func TestCaptureDay(t *testing.T) {
	for in, want := range map[string]string{
		"20260503T101530": "2026-05-03",
		"20260503":        "2026-05-03",
		"":                "",
		"notadate":        "",
	} {
		if got := CaptureDay(in); got != want {
			t.Fatalf("CaptureDay(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDeviceInfo(t *testing.T) {
	// build a synthetic DeviceInfo dataset
	var b []byte
	u16 := func(v uint16) { b = append(b, byte(v), byte(v>>8)) }
	u32 := func(v uint32) { b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	str := func(s string) {
		b = append(b, byte(len(s)+1))
		for _, r := range s {
			u16(uint16(r))
		}
		u16(0)
	}
	u16(100)        // standard version
	u32(0x6)        // vendor ext id
	u16(110)        // vendor ext version
	str("fuji ext") // vendor ext desc
	u16(0)          // functional mode
	for k := 0; k < 5; k++ { // five u16 arrays with two entries each
		u32(2)
		u16(0x1001)
		u16(0x1002)
	}
	str("FUJIFILM")
	str("X-H2S")
	str("1.10")
	str("21AQ00123")

	di, err := ParseDeviceInfo(b)
	if err != nil {
		t.Fatal(err)
	}
	if di.Manufacturer != "FUJIFILM" || di.Model != "X-H2S" || di.Serial != "21AQ00123" {
		t.Fatalf("bad parse: %+v", di)
	}
}

// A refusal must be legible. The response container was being discarded, so
// every refusal — busy, unsupported, bad property — surfaced identically as
// "returned no data".
func TestParseResponse(t *testing.T) {
	container := func(code uint16, txn uint32, params ...uint32) []byte {
		b := make([]byte, 12+4*len(params))
		binary.LittleEndian.PutUint32(b[0:], uint32(len(b)))
		binary.LittleEndian.PutUint16(b[4:], 3) // response
		binary.LittleEndian.PutUint16(b[6:], code)
		binary.LittleEndian.PutUint32(b[8:], txn)
		for i, p := range params {
			binary.LittleEndian.PutUint32(b[12+4*i:], p)
		}
		return b
	}

	r, err := ParseResponse(container(RespDeviceBusy, 42, 7))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if r.Code != RespDeviceBusy || r.Txn != 42 {
		t.Errorf("got code=0x%04x txn=%d", r.Code, r.Txn)
	}
	if len(r.Params) != 1 || r.Params[0] != 7 {
		t.Errorf("params = %v, want [7]", r.Params)
	}
	if r.OK() {
		t.Error("DeviceBusy must not report OK")
	}
	if got := r.String(); got != "DeviceBusy (0x2019)" {
		t.Errorf("String() = %q", got)
	}

	if r, err := ParseResponse(container(RespOK, 1)); err != nil || !r.OK() {
		t.Errorf("OK container: %+v err=%v", r, err)
	}

	// An unnamed code still has to log usefully — the raw number is the point.
	r, _ = ParseResponse(container(0x2FFF, 3))
	if got := r.String(); got != "unknown (0x2fff)" {
		t.Errorf("unnamed code rendered %q", got)
	}

	// Hosts differ in how much of the container they hand back; a bare code is
	// still worth having rather than discarding the one fact we came for.
	bare := []byte{0x19, 0x20}
	if r, err := ParseResponse(bare); err != nil || r.Code != RespDeviceBusy {
		t.Errorf("bare code: %+v err=%v", r, err)
	}

	// A data container is not a response; say so instead of reading a code out
	// of the wrong offset.
	data := container(RespOK, 1)
	binary.LittleEndian.PutUint16(data[4:], 2)
	if _, err := ParseResponse(data); err == nil {
		t.Error("a data container should not parse as a response")
	}
	if _, err := ParseResponse([]byte{1, 2, 3}); err == nil {
		t.Error("a runt buffer should not parse")
	}

	// Truncated container: the length field claims more than we were given.
	trunc := container(RespStoreFull, 9, 1, 2, 3)
	binary.LittleEndian.PutUint32(trunc[0:], 999)
	if r, err := ParseResponse(trunc); err != nil || r.Code != RespStoreFull {
		t.Errorf("truncated container: %+v err=%v", r, err)
	}
}
