package cull

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/photo"
	"github.com/zack/fuji-tools/internal/pipeline"
)

// Importer runs the keep-list through copy -> restamp -> hash -> Immich.
// One import at a time; status is polled by the UI.
type Importer struct {
	mu     sync.Mutex
	status ImportStatus
}

type ImportStatus struct {
	Running bool   `json:"running"`
	Phase   string `json:"phase"` // idle | copy | upload | validate | done | error
	Done    int    `json:"done"`  // files copied off the camera
	// Uploaded counts files pushed to Immich. It is separate from Done
	// because the two now run at the same time: sharing one counter made the
	// number jump between copy and upload progress.
	Uploaded int `json:"uploaded"`
	Total    int `json:"total"`
	// The upload currently worth showing — a large video otherwise looks like
	// a stalled import for minutes at a time — plus the current rate.
	File       string  `json:"file,omitempty"`
	FileSent   int64   `json:"fileSent,omitempty"`
	FileTotal  int64   `json:"fileTotal,omitempty"`
	RateBps    float64 `json:"rateBps,omitempty"`
	Message    string  `json:"message"`
	Error      string  `json:"error"`
	Dest       string  `json:"dest"`
	StartedAt  string  `json:"startedAt,omitempty"`
	FinishedAt string  `json:"finishedAt,omitempty"`
}

func (im *Importer) Status() ImportStatus {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.status
}

func (im *Importer) update(fn func(*ImportStatus)) {
	im.mu.Lock()
	fn(&im.status)
	im.mu.Unlock()
}

// stageBufferAhead is how many copied-but-not-yet-uploaded files may sit in
// the staging directory at once. The camera pull blocks past this, so disk use
// stays flat regardless of how many shots are being imported. Chunked camera
// batches mean the real peak is this plus one batch.
const stageBufferAhead = 50

// stageBufferBytes caps that buffer by size as well. Fifty 25 MB JPEGs is a
// gigabyte; fifty videos could be a hundred. Whichever limit is reached first
// stalls the camera pull, so the footprint is bounded either way.
//
// A file is admitted whenever the buffer is under this, even if it takes it
// over — so a large video starts copying alongside the photos already in
// flight instead of waiting for them to drain. Peak disk is therefore this
// plus one file, not a hard ceiling.
const stageBufferBytes = 8 << 30 // 8 GiB

// ImportOptions are the per-run choices offered in the import panel.
type ImportOptions struct {
	// Immich uploads to the configured server. Off imports to disk only.
	Immich bool
	// Reimport includes shots already imported. Off by default: the common
	// case is a fresh event, not re-sending a finished one.
	Reimport bool
	// KeepLocal keeps the copies in dest. Off makes the copy a staging step:
	// files land in a temp directory and are removed once the upload has been
	// verified against the server, so nothing is deleted on trust.
	KeepLocal bool
}

// keeperFile is one camera file belonging to a kept shot.
type keeperFile struct {
	shot *photo.Shot
	ext  string
}

// Start kicks off an import of the current keepers in the background.
func (im *Importer) Start(app *App, dest, album string, opt ImportOptions) error {
	if !opt.Immich && !opt.KeepLocal {
		return fmt.Errorf("nothing to do: uploading is off and local copies are not being kept")
	}
	if opt.Immich {
		if _, _, _, _, active := app.ImmichSettings(); !active {
			return fmt.Errorf("Immich upload is on but no server/key is configured — add them in settings, or turn uploading off")
		}
	}
	if opt.KeepLocal && dest == "" {
		return fmt.Errorf("no destination configured; pass --dest at startup or in the import request")
	}
	keepers, skipped := app.keeperFiles(opt.Reimport)
	if len(keepers) == 0 {
		if skipped > 0 {
			return fmt.Errorf("every kept shot has already been imported (%d) — tick \"re-import already imported\" to send them again", skipped)
		}
		return fmt.Errorf("no shots marked as keep")
	}
	if skipped > 0 {
		log.Printf("import: %d shot(s) already imported — skipping", skipped)
	}

	im.mu.Lock()
	if im.status.Running {
		im.mu.Unlock()
		return fmt.Errorf("an import is already running")
	}
	if opt.KeepLocal {
		saveImportDefaults(dest, album) // prefill the panel next session
	}
	im.status = ImportStatus{
		Running:   true,
		Phase:     "copy",
		Total:     len(keepers),
		Dest:      dest,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	im.mu.Unlock()

	go im.run(app, dest, album, keepers, opt)
	return nil
}

func (im *Importer) run(app *App, dest, album string, keepers []keeperFile, opt ImportOptions) {
	// The camera link is single-threaded: wait out any in-flight prefetch,
	// then own the link for the whole copy phase.
	app.prefetch.PauseAndDrain()
	defer app.prefetch.Resume()

	// Upload-only: stage into a temp directory that is removed once the
	// server has confirmed every file. Nothing is deleted before that.
	staging := ""
	if !opt.KeepLocal {
		tmp, terr := os.MkdirTemp(app.prefetch.cache, "import-stage-")
		if terr != nil {
			im.update(func(s *ImportStatus) {
				s.Running, s.Phase = false, "error"
				s.Error = fmt.Sprintf("staging dir: %v", terr)
				s.FinishedAt = time.Now().Format(time.RFC3339)
			})
			return
		}
		staging, dest = tmp, tmp
	}

	opts := app.pipeline()
	opts.Dest = dest
	opts.ImmichAlbum = album
	if !opt.Immich {
		opts.SkipImmich = true
	}
	if staging != "" {
		// Staged copies are scratch: keep only a working set on disk and drop
		// each file as the server takes it, so a 900-shot upload-only import
		// costs a few GB of disk instead of all of it.
		opts.BufferAhead = stageBufferAhead
		opts.BufferBytes = stageBufferBytes
		opts.DeleteAfterUpload = true
	}
	opts.FileProgress = func(name string, sent, total int64, bps float64) {
		im.update(func(s *ImportStatus) {
			s.File, s.FileSent, s.FileTotal, s.RateBps = name, sent, total, bps
		})
	}
	opts.Progress = func(phase string, done, total int) {
		im.update(func(s *ImportStatus) {
			if phase == "upload" {
				s.Uploaded = done // copy owns Done; these advance together
				return
			}
			s.Phase = phase
		})
	}

	var files []photo.FileEntry
	ctx := context.Background()
	// Hash and upload run alongside the camera pull rather than after it: the
	// two use different resources, and serialising them doubled import time.
	stream, err := pipeline.NewStreamer(ctx, opts, len(keepers))
	if err == nil {
		files, err = im.copyPhase(app, dest, keepers, stream.Add)
		if err == nil && opt.Immich {
			im.update(func(s *ImportStatus) { s.Phase = "upload" })
		}
		// Always drain the workers, even when the pull failed part-way, or
		// their goroutines would outlive the import.
		if werr := stream.Wait(); err == nil {
			err = werr
		}
	}
	if staging != "" {
		if err == nil {
			os.RemoveAll(staging) // verified on the server; the copy was scratch
		} else {
			log.Printf("import: staged copies kept at %s (upload did not verify)", staging)
		}
	}

	im.update(func(s *ImportStatus) {
		s.File, s.FileSent, s.FileTotal, s.RateBps = "", 0, 0, 0
		s.Running = false
		s.FinishedAt = time.Now().Format(time.RFC3339)
		if err != nil {
			s.Phase = "error"
			s.Error = err.Error()
		} else {
			s.Phase = "done"
			switch {
			case staging != "":
				s.Message = fmt.Sprintf("%d files uploaded to Immich (no local copy kept)", len(files))
			case opt.Immich:
				s.Message = fmt.Sprintf("%d files imported to %s and uploaded", len(files), dest)
			default:
				s.Message = fmt.Sprintf("%d files imported to %s", len(files), dest)
			}
		}
	})
	if err != nil {
		log.Printf("import failed: %v", err)
	} else {
		log.Printf("import complete: %d files -> %s", len(files), dest)
		// "keep" is a queue: mark these done so the next event starts clean.
		var ckeys []string
		for _, k := range keepers {
			if ck := app.catalog.CanonicalOf(k.shot.ID); ck != "" {
				ckeys = append(ckeys, ck)
			}
		}
		if err := app.session.MarkImported(ckeys, time.Now()); err != nil {
			log.Printf("WARN: recording imported shots: %v", err)
		}
		if app.imcheck != nil {
			// the pipeline just validated these on the server: badge them
			seen := map[string]bool{}
			var ids []string
			for _, k := range keepers {
				if !seen[k.shot.ID] {
					seen[k.shot.ID] = true
					ids = append(ids, k.shot.ID)
				}
			}
			app.imcheck.MarkUploaded(ids)
		}
	}
}

// copyPhase lands keeper files in dest/<folder>/: files already present are
// kept, cached camera-verbatim copies (prefetched JPGs/RAFs/videos) are
// copied locally, and the remainder is pulled from the camera in per-folder
// batches. Returns the FileEntry list for the pipeline.
func (im *Importer) copyPhase(app *App, dest string, keepers []keeperFile, onFile func(photo.FileEntry)) ([]photo.FileEntry, error) {
	type pullItem struct {
		it   fetchItem
		size int64
		kind string
		idx  int // into files, so a landed pull can be handed straight on
	}
	files := make([]photo.FileEntry, len(keepers))
	var toPull []pullItem
	done := 0

	for i, k := range keepers {
		name := k.shot.Files[k.ext]
		target := filepath.Join(dest, k.shot.Folder, name)
		files[i] = photo.FileEntry{Folder: k.shot.Folder, Name: name, LocalPath: target}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}

		wantSize := k.shot.Sizes[k.ext]
		if st, err := os.Stat(target); err == nil && st.Size() > 0 && (wantSize == 0 || st.Size() == wantSize) {
			done++
			im.update(func(s *ImportStatus) { s.Done = done })
			onFile(files[i])
			continue
		}
		if cached, ok := app.prefetch.CachedFile(k.shot, k.ext); ok {
			tmp := target + ".tmp"
			if err := copyFile(cached, tmp); err == nil && commit(tmp, target) == nil {
				done++
				im.update(func(s *ImportStatus) { s.Done = done })
				onFile(files[i])
				continue
			}
			os.Remove(tmp)
		}
		kind := "jpg"
		switch {
		case k.ext == "RAF":
			kind = "raf"
		case photo.IsHEIF(k.ext):
			kind = "heif"
		case k.shot.Kind == "video":
			kind = "mov"
		}
		toPull = append(toPull, pullItem{
			it: fetchItem{
				CameraDir: k.shot.CameraDir, Name: name,
				ObjectID: k.shot.ObjectIDs[k.ext], Dest: target + ".tmp",
			},
			size: wantSize, kind: kind, idx: i,
		})
	}

	// Pull in bounded chunks with per-file validation and retry. One
	// unbounded 859-file session once wedged an import forever, and its
	// first hiccup aborted the whole run — every landed file is committed
	// immediately, so failures and re-runs only ever resume.
	const importChunk = 24
	if len(toPull) > 0 {
		log.Printf("import: pulling %d files from camera (%d satisfied locally)", len(toPull), done)
	}
	pending := toPull
	for round := 1; len(pending) > 0 && round <= 3; round++ {
		if round > 1 {
			log.Printf("import: retrying %d files (round %d/3)", len(pending), round)
			time.Sleep(10 * time.Second)
		}
		var failed []pullItem
		for start := 0; start < len(pending); start += importChunk {
			end := start + importChunk
			if end > len(pending) {
				end = len(pending)
			}
			chunk := pending[start:end]
			items := make([]fetchItem, len(chunk))
			for i, c := range chunk {
				items[i] = c.it
			}
			ctx, cancel := context.WithTimeout(context.Background(),
				60*time.Second+time.Duration(len(items))*15*time.Second)
			// Ride the persistent partial-read session when there is one —
			// the same path browsing uses. A one-shot aft per chunk is what
			// trips this camera into replaying stale buffers, which is why an
			// import could fail on a card that browsed perfectly.
			var fetchErr error
			if app.prefetch.partsOK() {
				sizes := make([]int64, len(chunk))
				for i, c := range chunk {
					sizes[i] = c.size
				}
				fetchErr = app.prefetch.fetchItemsViaParts(ctx, items, sizes)
			} else {
				fetchErr = app.backend.Fetch(ctx, items)
			}
			cancel()
			if fetchErr != nil {
				log.Printf("import: chunk of %d: %v", len(items), fetchErr)
			}
			garbage := 0
			for _, c := range chunk {
				target := strings.TrimSuffix(c.it.Dest, ".tmp")
				st, err := os.Stat(c.it.Dest)
				complete := err == nil && st.Size() > 0 && (c.size == 0 || st.Size() == c.size)
				if complete && !mediaValid(c.it.Dest, c.kind) {
					garbage++
					complete = false
				}
				if !complete {
					os.Remove(c.it.Dest)
					failed = append(failed, c)
					continue
				}
				if err := commit(c.it.Dest, target); err != nil {
					return nil, err
				}
				done++
				im.update(func(s *ImportStatus) { s.Done = done })
				onFile(files[c.idx])
			}
			if garbage > 0 {
				// Trip the breaker so the UIs show CAMERA SICK; an
				// all-garbage chunk means every further pull is wasted —
				// only a power cycle cures the stale-buffer state.
				log.Printf("import: %d/%d transfers in chunk were stale-buffer garbage — POWER-CYCLE the camera", garbage, len(chunk))
				app.prefetch.mu.Lock()
				app.prefetch.bulkSick, app.prefetch.bulkSickAt = true, time.Now()
				app.prefetch.mu.Unlock()
				if garbage == len(chunk) {
					return nil, fmt.Errorf("camera is replaying stale buffers for every transfer — power-cycle it, then run import again (everything already copied is kept)")
				}
			}
		}
		pending = failed
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("camera pull: %d files failed after 3 attempts (first: %s/%s) — everything else is copied; run import again to resume",
			len(pending), pending[0].it.CameraDir, pending[0].it.Name)
	}
	return files, nil
}

func commit(tmp, target string) error {
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// keeperFiles expands kept shots into their individual camera files.
// keeperFiles returns the files to import. Shots already imported are skipped
// unless includeDone, so "keep" behaves as a queue: finish one event and the
// next import carries only what is new. Without this, a second import re-pulls
// everything from the camera and — because a duplicate upload still returns an
// asset id — files the whole previous event into the new album.
func (a *App) keeperFiles(includeDone bool) (files []keeperFile, skipped int) {
	decisions := a.session.Decisions()
	done := a.session.ImportedKeys()
	seenSkipped := map[string]bool{}
	var out []keeperFile
	for _, s := range a.catalog.Shots {
		if decisions[s.ID] != "keep" {
			continue
		}
		if !includeDone {
			if k := a.catalog.CanonicalOf(s.ID); k != "" && done[k] != "" {
				if !seenSkipped[s.ID] {
					seenSkipped[s.ID] = true
					skipped++
				}
				continue
			}
		}
		exts := make([]string, 0, len(s.Files))
		for ext := range s.Files {
			exts = append(exts, ext)
		}
		sort.Strings(exts)
		for _, ext := range exts {
			out = append(out, keeperFile{shot: s, ext: ext})
		}
	}
	return out, skipped
}

// PendingImport reports how many shots an import would carry, and how many are
// held back as already imported — so the panel can say so before you start.
func (a *App) PendingImport(includeDone bool) (shots, skipped int) {
	files, skipped := a.keeperFiles(includeDone)
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.shot.ID] = true
	}
	return len(seen), skipped
}
