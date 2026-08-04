package mtppart

import (
	"crypto/sha1"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestLivePartialReadMatchesWholeObject reads real objects through the
// persistent partial-read session and reports size + hash, so the result can
// be compared against a one-shot get of the same object. The partial-read path
// is only trustworthy if it returns the same bytes.
//
// Live hardware only: set FUJI_LIVE_OBJ="<id>:<name>:<size>,..." to run.
func TestLivePartialReadMatchesWholeObject(t *testing.T) {
	spec := os.Getenv("FUJI_LIVE_OBJ")
	if spec == "" {
		t.Skip("set FUJI_LIVE_OBJ=<id>:<name>:<size>[,...] to run against a camera")
	}
	srv, err := StartServer()
	if err != nil {
		t.Fatalf("start serve-parts: %v", err)
	}
	defer srv.Close()

	for _, one := range strings.Split(spec, ",") {
		f := strings.Split(one, ":")
		if len(f) != 3 {
			t.Fatalf("bad spec %q", one)
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		h := sha1.New()
		var off int64
		short, reads := 0, 0
		const chunk = 8 << 20
		for {
			want := int64(chunk)
			if size > 0 && off+want > size {
				want = size - off
			}
			if want <= 0 {
				break
			}
			data, err := srv.ReadAt(f[0], off, want)
			reads++
			if err != nil {
				t.Errorf("%s: read error at offset %d: %v", f[1], off, err)
				break
			}
			if len(data) == 0 {
				break
			}
			if int64(len(data)) < want {
				short++
			}
			h.Write(data)
			off += int64(len(data))
		}
		status := "COMPLETE"
		if off != size {
			status = "SHORT"
		}
		fmt.Printf("  %-14s %s %d/%d bytes in %d reads (%d short) sha1=%x\n",
			f[1], status, off, size, reads, short, h.Sum(nil)[:8])
		if off != size {
			t.Errorf("%s: partial-read session returned %d of %d bytes", f[1], off, size)
		}
	}
}
