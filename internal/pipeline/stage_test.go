package pipeline

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
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

	"github.com/zack/fuji-tools/internal/immich"
	"github.com/zack/fuji-tools/internal/photo"
)

// The stack lane's denominator is complete RAF+JPG pairs, not files and not
// shots. An import carrying JPG-only shots or videos would otherwise leave the
// bar permanently short of its own end.
func TestStackStageCountsOnlyCompletePairs(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	files := []photo.FileEntry{
		{Folder: "151_FUJI", Name: "DSCF0001.JPG", AssetID: "a1"},
		{Folder: "151_FUJI", Name: "DSCF0001.RAF", AssetID: "a2"},
		{Folder: "151_FUJI", Name: "DSCF0002.JPG", AssetID: "a3"},
		{Folder: "151_FUJI", Name: "DSCF0002.RAF", AssetID: "a4"},
		{Folder: "151_FUJI", Name: "DSCF0003.JPG", AssetID: "a5"}, // JPG only
		{Folder: "151_FUJI", Name: "DSCF0004.MOV", AssetID: "a6"}, // video
	}

	var last Stage
	var sawTotals []int
	opts := Options{StageProgress: func(name string, st Stage) {
		if name != StageStack {
			t.Fatalf("unexpected stage %q", name)
		}
		last = st
		sawTotals = append(sawTotals, st.FilesTotal)
	}}
	StackPairs(context.Background(), opts, immich.NewClient(srv.URL, "k"), files)

	if posts != 2 {
		t.Errorf("CreateStack calls = %d, want 2", posts)
	}
	if last.FilesTotal != 2 {
		t.Errorf("FilesTotal = %d, want 2 (complete pairs only)", last.FilesTotal)
	}
	if last.Files != 2 {
		t.Errorf("Files = %d, want 2", last.Files)
	}
	if last.Failed != 0 {
		t.Errorf("Failed = %d, want 0", last.Failed)
	}
	if !last.Done {
		t.Error("final stack stage should be Done")
	}
	for i, tot := range sawTotals {
		if tot != 2 {
			t.Errorf("update %d reported FilesTotal %d, want a stable 2", i, tot)
		}
	}
}

// With no pairs at all the lane must still resolve, or a JPG-only import ends
// with a stack lane that never leaves "running".
func TestStackStageResolvesWithNoPairs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("CreateStack must not be called when there are no pairs")
	}))
	defer srv.Close()

	var last Stage
	var updates int
	opts := Options{StageProgress: func(_ string, st Stage) { last, updates = st, updates+1 }}
	StackPairs(context.Background(), opts, immich.NewClient(srv.URL, "k"),
		[]photo.FileEntry{{Folder: "151_FUJI", Name: "DSCF0001.JPG", AssetID: "a1"}})

	if updates == 0 {
		t.Fatal("no stage update reported")
	}
	if !last.Done || last.FilesTotal != 0 {
		t.Errorf("got %+v, want Done with FilesTotal 0", last)
	}
}

// A failed upload must never read as progress: it belongs in Failed, and Files
// must stay at the count the server actually accepted. Before this, a run where
// every upload failed finished with a full bar.
func TestUploadStageKeepsFailuresOutOfProgress(t *testing.T) {
	var got Stage
	s := &Streamer{
		total:    10,
		opts:     Options{TotalBytes: 1000, StageProgress: func(_ string, st Stage) { got = st }},
		inflight: map[string]*fileProg{},
		ok:       3,
		dup:      1,
		fail:     2,
		upBytes:  400,
	}
	s.emitUpload()

	if got.Files != 4 {
		t.Errorf("Files = %d, want 4 (accepted: ok+dup)", got.Files)
	}
	if got.Failed != 2 {
		t.Errorf("Failed = %d, want 2", got.Failed)
	}
	if got.FilesTotal != 10 || got.BytesTotal != 1000 {
		t.Errorf("denominators = %d/%d, want 10/1000", got.FilesTotal, got.BytesTotal)
	}
	if got.Bytes != 400 {
		t.Errorf("Bytes = %d, want 400", got.Bytes)
	}
}

// The whole stage timeline across a failed upload and its retry — the shape the
// clients render. Extends the httptest Immich stub the other pipeline tests use
// rather than standing up a server of its own.
func TestStageTimelineAcrossAFailedUploadAndRetry(t *testing.T) {
	dir := t.TempDir()
	const doomed = "DSCF0003.RAF"

	var mu sync.Mutex
	have := map[string]string{} // checksum -> assetID
	failedOnce := false
	stacks := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/bulk-upload-check"):
			var req struct {
				Assets []struct{ ID, Checksum string } `json:"assets"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			defer mu.Unlock()
			var out struct {
				Results []map[string]string `json:"results"`
			}
			for _, a := range req.Assets {
				if id, ok := have[a.Checksum]; ok {
					out.Results = append(out.Results, map[string]string{
						"id": a.ID, "action": "reject", "reason": "duplicate", "assetId": id})
				} else {
					out.Results = append(out.Results, map[string]string{"id": a.ID, "action": "accept"})
				}
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/stacks"):
			io.Copy(io.Discard, r.Body)
			mu.Lock()
			stacks++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default: // upload
			h := sha1.New()
			name := ""
			if mr, err := r.MultipartReader(); err == nil {
				for {
					p, err := mr.NextPart()
					if err != nil {
						break
					}
					if p.FormName() == "assetData" {
						name = p.FileName()
						io.Copy(h, p)
					} else {
						io.Copy(io.Discard, p)
					}
				}
			}
			mu.Lock()
			if name == doomed && !failedOnce {
				failedOnce = true
				mu.Unlock()
				http.Error(w, `{"message":"synthetic"}`, http.StatusInternalServerError)
				return
			}
			csum := base64.StdEncoding.EncodeToString(h.Sum(nil))
			id, seen := have[csum]
			if !seen {
				id = fmt.Sprintf("asset-%d", len(have)+1)
				have[csum] = id
			}
			mu.Unlock()
			status := "created"
			if seen {
				status = "duplicate"
			}
			json.NewEncoder(w).Encode(map[string]string{"id": id, "status": status})
		}
	}))
	defer srv.Close()

	const pairs = 6
	var entries []photo.FileEntry
	var totalBytes int64
	for i := 1; i <= pairs; i++ {
		// RAFs an order of magnitude larger than JPGs, as on the card — a file
		// count cannot see that asymmetry, which is why the lanes carry bytes.
		for ext, size := range map[string]int{"JPG": 8 << 10, "RAF": 96 << 10} {
			name := fmt.Sprintf("DSCF%04d.%s", i, ext)
			body := make([]byte, size)
			for j := range body {
				body[j] = byte((j*7 + i*131 + int(ext[0])) % 251) // unique per file
			}
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, body, 0o644); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, photo.FileEntry{Folder: "151_FUJI", Name: name, LocalPath: p})
			totalBytes += int64(size)
		}
	}

	type sample struct {
		stage string
		st    Stage
	}
	var tl []sample
	var tlMu sync.Mutex
	opts := Options{
		ImmichURL: srv.URL, ImmichKey: "k", Dest: dir,
		UploadConcurrency: 2, Retries: 2, ImmichStack: true,
		TotalBytes: totalBytes,
		StageProgress: func(name string, st Stage) {
			tlMu.Lock()
			tl = append(tl, sample{name, st})
			tlMu.Unlock()
		},
	}
	s, err := NewStreamer(context.Background(), opts, len(entries))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s.Add(e)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	tlMu.Lock()
	defer tlMu.Unlock()
	last := map[string]Stage{}
	for _, s := range tl {
		last[s.stage] = s.st
	}

	// The bug that started all this: the upload lane must never read full while
	// files remain, whatever the camera lane is doing.
	for _, s := range tl {
		if s.stage == StageUpload && s.st.Files == s.st.FilesTotal && s.st.Files < len(entries) {
			t.Errorf("upload lane read %d/%d — full while incomplete",
				s.st.Files, s.st.FilesTotal)
			break
		}
	}
	// Retries used to drive progress through the package-level Upload, whose
	// denominator is len(missing) — the counter collapsed to 1/1 mid-run.
	for _, s := range tl {
		if s.stage == StageUpload && s.st.FilesTotal != len(entries) {
			t.Fatalf("upload denominator moved to %d, want a stable %d",
				s.st.FilesTotal, len(entries))
		}
	}
	sawFailure := false
	for _, s := range tl {
		if s.stage == StageUpload && s.st.Failed > 0 {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("the deliberate upload failure never surfaced on the lane")
	}
	if up := last[StageUpload]; up.Failed != 0 || up.Files != len(entries) {
		t.Errorf("final upload = %d/%d failed=%d; the retry should have cleared it",
			up.Files, up.FilesTotal, up.Failed)
	}
	if up := last[StageUpload]; !up.Done {
		t.Error("upload lane never reached Done — a finished import still reads as running")
	}
	// The retry runs through the package-level Upload, which does not touch the
	// byte counter — so a clean import used to finish with the bar short by
	// exactly the retried file.
	if up := last[StageUpload]; up.Bytes != totalBytes {
		t.Errorf("final upload bytes = %d, want %d (short by %d — retried bytes uncredited)",
			up.Bytes, totalBytes, totalBytes-up.Bytes)
	}
	if v := last[StageVerify]; v.Files != len(entries) || !v.Done {
		t.Errorf("final verify = %d/%d done=%v", v.Files, v.FilesTotal, v.Done)
	}
	if k := last[StageStack]; k.Files != pairs || k.FilesTotal != pairs || !k.Done {
		t.Errorf("final stack = %d/%d done=%v, want %d pairs", k.Files, k.FilesTotal, k.Done, pairs)
	}
	// Stacking must be observable in flight, not one jump from nothing to done.
	mid := 0
	for _, s := range tl {
		if s.stage == StageStack && s.st.Files > 0 && s.st.Files < pairs {
			mid++
		}
	}
	if mid < 3 {
		t.Errorf("stack lane had %d intermediate samples, want >=3", mid)
	}
	if stacks != pairs {
		t.Errorf("server saw %d stack calls, want %d", stacks, pairs)
	}
}
