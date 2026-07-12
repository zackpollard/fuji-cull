// Package exif restamps file mtimes from capture metadata and extracts RAF
// previews — in pure Go. It replaced an exiftool (Perl) dependency, which
// Android cannot carry and other platforms are happier without.
package exif

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zack/fuji-tools/internal/jpegmeta"
)

// EnsurePath is a no-op kept for call-site compatibility with the exiftool
// era (there is nothing external to find anymore).
func EnsurePath() error { return nil }

// RestampMtime sets file mtimes from EXIF DateTimeOriginal (JPG/RAF) or the
// QuickTime mvhd creation time (MOV/MP4), recursively over dir. Matching the
// previous exiftool behavior, timestamps are interpreted as local wall time.
// Best-effort per file; only the directory walk itself can fail.
func RestampMtime(dir string) error {
	restamped, skipped := 0, 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		var ts time.Time
		switch strings.ToUpper(filepath.Ext(path)) {
		case ".JPG", ".JPEG", ".RAF":
			ts = imageCaptureTime(path)
		case ".MOV", ".MP4":
			ts = quicktimeCreateTime(path)
		default:
			return nil
		}
		if ts.IsZero() {
			skipped++
			return nil
		}
		if err := os.Chtimes(path, ts, ts); err != nil {
			skipped++
			return nil
		}
		restamped++
		return nil
	})
	if err != nil {
		return fmt.Errorf("restamp %s: %w", dir, err)
	}
	if skipped > 0 {
		log.Printf("restamp: %d files stamped, %d without usable capture time", restamped, skipped)
	}
	return nil
}

// imageCaptureTime reads DateTimeOriginal from a JPG or RAF head.
func imageCaptureTime(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	head := make([]byte, 256<<10)
	n, _ := io.ReadFull(f, head)
	f.Close()
	if n < 16 {
		return time.Time{}
	}
	s := jpegmeta.DateTimeOriginal(head[:n])
	if s == "" {
		return time.Time{}
	}
	ts, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// quicktimeCreateTime walks the container's top-level boxes to moov and
// prefers Fuji's embedded EXIF date (the MVTG atom in udta — the START of
// recording in local wall time, which is what exiftool stamped), falling
// back to mvhd's creation time (end of recording, UTC). Boxes are skipped
// with seeks, so multi-GB files cost a few reads.
func quicktimeCreateTime(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	var off int64
	for {
		var hdr [16]byte
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return time.Time{}
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		box := string(hdr[4:8])
		payload := off + 8
		if size == 1 { // 64-bit box size
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return time.Time{}
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			payload = off + 16
		}
		if size < 8 {
			return time.Time{}
		}
		if box == "moov" {
			read := size
			if read > 16<<20 {
				read = 16 << 20
			}
			moov := make([]byte, read)
			if _, err := f.ReadAt(moov, off); err != nil {
				return time.Time{}
			}
			if s := fujiMovDateTime(moov); s != "" {
				if ts, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local); err == nil {
					return ts
				}
			}
			return mvhdCreateTime(f, payload, off+size)
		}
		off += size
	}
}

// fujiMovDateTime extracts the capture time from Fuji's MVTG atom: a
// headerless little-endian TIFF block whose value offsets are relative to an
// implicit base. The base is derived by anchoring the Make entry (tag 0x010F)
// to its "FUJIFILM" payload; the date then comes from DateTimeOriginal
// (0x9003, in the Exif sub-IFD) or ModifyDate (0x0132) — both ASCII count 20.
func fujiMovDateTime(moov []byte) string {
	i := bytes.Index(moov, []byte("MVTG"))
	if i < 0 {
		return ""
	}
	s := moov[i:]
	if len(s) > 16<<10 {
		s = s[:16<<10]
	}
	mk := bytes.Index(s, []byte{0x0f, 0x01, 0x02, 0x00}) // Make, ASCII
	fj := bytes.Index(s, []byte("FUJIFILM\x00"))
	if mk < 0 || fj < 0 || mk+12 > len(s) {
		return ""
	}
	base := fj - int(binary.LittleEndian.Uint32(s[mk+8:]))
	for _, pat := range [][]byte{
		{0x03, 0x90, 0x02, 0x00, 0x14, 0x00, 0x00, 0x00}, // DateTimeOriginal
		{0x32, 0x01, 0x02, 0x00, 0x14, 0x00, 0x00, 0x00}, // ModifyDate
	} {
		if p := bytes.Index(s, pat); p >= 0 && p+12 <= len(s) {
			off := base + int(binary.LittleEndian.Uint32(s[p+8:]))
			if off >= 0 && off+19 <= len(s) {
				return string(s[off : off+19])
			}
		}
	}
	return ""
}

func mvhdCreateTime(f *os.File, off, end int64) time.Time {
	for off+8 <= end {
		var hdr [8]byte
		if _, err := f.ReadAt(hdr[:], off); err != nil {
			return time.Time{}
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		if size < 8 {
			return time.Time{}
		}
		if string(hdr[4:8]) == "mvhd" {
			var body [20]byte
			if _, err := f.ReadAt(body[:], off+8); err != nil {
				return time.Time{}
			}
			var secs uint64
			if body[0] == 1 { // version 1: 64-bit times
				secs = binary.BigEndian.Uint64(body[4:12])
			} else {
				secs = uint64(binary.BigEndian.Uint32(body[4:8]))
			}
			if secs == 0 {
				return time.Time{}
			}
			// QuickTime epoch, re-interpreted as local wall time
			t := time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(secs) * time.Second)
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
		}
		off += size
	}
	return time.Time{}
}

// ExtractPreview writes the embedded full-resolution JPEG of a Fuji RAF to
// dst. The RAF header stores the preview's offset and length big-endian at
// bytes 84 and 88.
func ExtractPreview(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [92]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return fmt.Errorf("raf header %s: %w", src, err)
	}
	if string(hdr[:8]) != "FUJIFILM" {
		return fmt.Errorf("%s is not a RAF", src)
	}
	off := int64(binary.BigEndian.Uint32(hdr[84:88]))
	length := int64(binary.BigEndian.Uint32(hdr[88:92]))
	if off <= 0 || length < 1024 {
		return fmt.Errorf("no embedded preview in %s (offset %d, length %d)", src, off, length)
	}
	var magic [2]byte
	if _, err := f.ReadAt(magic[:], off); err != nil || magic[0] != 0xFF || magic[1] != 0xD8 {
		return fmt.Errorf("embedded preview in %s is not a JPEG", src)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(f, off, length)); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
