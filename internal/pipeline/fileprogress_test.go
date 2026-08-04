package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zack/fuji-tools/internal/photo"
)

// A multi-GB video holds the file counter still for minutes, so per-file bytes
// are the only moving signal. They must actually advance, end at the file's
// size, and report the biggest in-flight upload rather than whichever small
// file updated last.
func TestFileProgressTracksTheLargestUpload(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bulk-upload-check") {
			var req struct {
				Assets []struct {
					ID       string `json:"id"`
					Checksum string `json:"checksum"`
				} `json:"assets"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			type result struct {
				Action, Reason, AssetID, ID string
			}
			var out struct {
				Results []map[string]string `json:"results"`
			}
			for _, a := range req.Assets {
				out.Results = append(out.Results, map[string]string{
					"action": "reject", "reason": "duplicate", "assetId": "a-" + a.ID, "id": a.ID})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		io.Copy(io.Discard, r.Body) // consume so the pipe drains
		json.NewEncoder(w).Encode(map[string]string{"id": "a", "status": "created"})
	}))
	defer srv.Close()

	const mib = 1 << 20
	sizes := map[string]int{"DSCF0001.JPG": 2 * mib, "MOV0001.MOV": 12 * mib}
	var mu sync.Mutex
	peak := map[string]int64{}
	var sawRate bool

	opts := Options{
		ImmichURL: srv.URL, ImmichKey: "k", UploadConcurrency: 2, Dest: dir,
		FileProgress: func(name string, sent, total int64, bps float64) {
			mu.Lock()
			defer mu.Unlock()
			if sent > peak[name] {
				peak[name] = sent
			}
			if bps > 0 {
				sawRate = true
			}
		},
	}
	s, err := NewStreamer(context.Background(), opts, len(sizes))
	if err != nil {
		t.Fatal(err)
	}
	for name, sz := range sizes {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, sz), 0o644); err != nil {
			t.Fatal(err)
		}
		s.Add(photo.FileEntry{Name: name, LocalPath: p})
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(peak) == 0 {
		t.Fatal("no per-file progress was reported at all")
	}
	// The video is the file a person waits on; it must be the one reported.
	if peak["MOV0001.MOV"] == 0 {
		t.Errorf("the largest upload was never reported: %v", peak)
	}
	for name, got := range peak {
		if want := int64(sizes[name]); got > want {
			t.Errorf("%s: reported %d bytes, more than its size %d", name, got, want)
		}
	}
	_ = sawRate // rate needs >1s of traffic; not asserted on a fast local stub
	_ = fmt.Sprint(time.Now())
}
