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
	// Elapsed is computed on read rather than stored per update, so it keeps
	// ticking between progress events (a long single-file upload otherwise
	// freezes the clock along with the counter).
	startedAt  time.Time
	finishedAt time.Time
}

// ImportStage is one stage of an import, with its own counters.
//
// Copy, hash and upload overlap by design — that is what makes an import cost
// its slowest stage rather than the sum of them — so there is no single active
// phase to report and any number of these may be moving at once. Every client
// draws one lane per stage from these numbers instead of inferring a checklist
// from Phase.
type ImportStage struct {
	// State is pending | running | done, for lanes that have not started and
	// lanes that are finished.
	State string `json:"state"`
	Files int    `json:"files"`
	// FilesTotal counts PAIRS for the stack stage.
	FilesTotal int   `json:"filesTotal"`
	Bytes      int64 `json:"bytes,omitempty"`
	BytesTotal int64 `json:"bytesTotal,omitempty"`
	// Rate is bytes per second, or pairs per second for the stack stage.
	Rate float64 `json:"rate,omitempty"`
	// Cached counts files that were already on disk and never crossed the
	// camera link. Half a JPG+RAF import can land in the first instant; saying
	// so is what stops that looking like a broken counter.
	Cached int `json:"cached,omitempty"`
	// Failed is kept out of Files so a run where everything failed cannot
	// render as a full bar.
	Failed int `json:"failed,omitempty"`
}

type ImportStatus struct {
	Running bool   `json:"running"`
	Phase   string `json:"phase"` // idle | copy | upload | validate | done | error
	Done    int    `json:"done"`  // files copied off the camera
	// Uploaded counts files ACCEPTED by Immich (new or duplicate). It is
	// separate from Done because the two run at the same time: sharing one
	// counter made the number jump between copy and upload progress.
	Uploaded int `json:"uploaded"`
	Total    int `json:"total"`
	// The stage lanes. Phase and the three counters above are the pre-stage
	// shape, kept while every client migrates.
	Camera ImportStage `json:"camera"`
	Upload ImportStage `json:"upload"`
	Verify ImportStage `json:"verify"`
	Stack  ImportStage `json:"stack"`
	// ElapsedSec is wall-clock seconds since the run started, so a client can
	// show it without parsing StartedAt and without a clock of its own.
	ElapsedSec int `json:"elapsedSec,omitempty"`
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
	st := im.status
	if !im.startedAt.IsZero() {
		end := time.Now()
		if !im.finishedAt.IsZero() {
			end = im.finishedAt // a finished run stops counting
		}
		st.ElapsedSec = int(end.Sub(im.startedAt).Seconds())
	}
	return st
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
	var totalBytes int64
	for _, k := range keepers {
		totalBytes += k.shot.Sizes[k.ext]
	}
	// The stack lane's denominator is known here, from the keep list — waiting
	// for StackPairs to compute it left the lane reading "0 / 0 pairs" for the
	// whole import, which looks broken rather than pending.
	exts := map[string]map[string]bool{}
	for _, k := range keepers {
		if exts[k.shot.ID] == nil {
			exts[k.shot.ID] = map[string]bool{}
		}
		exts[k.shot.ID][k.ext] = true
	}
	pairs := 0
	for _, e := range exts {
		if e["JPG"] && e["RAF"] {
			pairs++
		}
	}
	now := time.Now()
	im.startedAt, im.finishedAt = now, time.Time{}
	im.status = ImportStatus{
		Running:   true,
		Phase:     "copy",
		Total:     len(keepers),
		Dest:      dest,
		StartedAt: now.Format(time.RFC3339),
		Camera:    ImportStage{State: "running", FilesTotal: len(keepers), BytesTotal: totalBytes},
		Upload:    ImportStage{State: "pending", FilesTotal: len(keepers), BytesTotal: totalBytes},
		Verify:    ImportStage{State: "pending", FilesTotal: len(keepers)},
		Stack:     ImportStage{State: "pending", FilesTotal: pairs},
	}
	if !opt.Immich {
		// Nothing downstream of the copy will run; saying "pending" forever
		// would be a lane waiting on something that never comes.
		im.status.Upload.State = "n/a"
		im.status.Verify.State = "n/a"
		im.status.Stack.State = "n/a"
	} else if !app.pipeline().ImmichStack || pairs == 0 {
		// Same for stacking specifically: off, or nothing to pair.
		im.status.Stack.State = "n/a"
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

	// Upload-only: stage into a fixed directory that is removed once the
	// server has confirmed every file. Nothing is deleted before that.
	//
	// Fixed, not os.MkdirTemp. copyPhase reuses anything already sitting at
	// the target path with a matching size, which is what lets a failed import
	// resume — but a fresh temp directory per run put those files somewhere the
	// next run could never look. So an upload-only import that died part-way
	// re-pulled every RAF from the camera, and left its staged copies behind
	// forever: one orphaned directory per failure, holding the bytes that would
	// have made the retry free.
	//
	// Only one import runs at a time (Start refuses a second), and the cache is
	// already keyed per camera, so a single well-known path is safe.
	staging := ""
	if !opt.KeepLocal {
		sweepStaleStaging(app.prefetch.cache)
		fixed := filepath.Join(app.prefetch.cache, "import-stage")
		if terr := os.MkdirAll(fixed, 0o755); terr != nil {
			im.update(func(s *ImportStatus) {
				s.Running, s.Phase = false, "error"
				s.Error = fmt.Sprintf("staging dir: %v", terr)
				s.FinishedAt = time.Now().Format(time.RFC3339)
			})
			return
		}
		staging, dest = fixed, fixed
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
	var totalBytes int64
	for _, k := range keepers {
		totalBytes += k.shot.Sizes[k.ext]
	}
	opts.TotalBytes = totalBytes
	opts.StageProgress = func(name string, st pipeline.Stage) {
		im.update(func(s *ImportStatus) {
			lane := &s.Upload
			switch name {
			case pipeline.StageVerify:
				lane = &s.Verify
			case pipeline.StageStack:
				lane = &s.Stack
			case pipeline.StageCamera:
				lane = &s.Camera
			}
			state := "running"
			if st.Done {
				state = "done"
			}
			*lane = ImportStage{
				State: state, Files: st.Files, FilesTotal: st.FilesTotal,
				Bytes: st.Bytes, BytesTotal: st.BytesTotal, Rate: st.Rate,
				Cached: st.Cached, Failed: st.Failed,
			}
		})
	}

	var files []photo.FileEntry
	ctx := context.Background()
	// Hash and upload run alongside the camera pull rather than after it: the
	// two use different resources, and serialising them doubled import time.
	stream, err := pipeline.NewStreamer(ctx, opts, len(keepers))
	if err == nil {
		files, err = im.copyPhase(app, dest, keepers, totalBytes, stream.Add)
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
			log.Printf("import: staged copies kept at %s — run import again to resume from them", staging)
		}
	}

	// Report what the server did, not how many files were attempted. Album
	// adds and stacking are non-fatal, so a run can finish with err == nil and
	// still have failures worth naming.
	upOK, upDup, upFail := 0, 0, 0
	if stream != nil {
		upOK, upDup, upFail = stream.Counts()
	}
	im.mu.Lock()
	im.finishedAt = time.Now()
	im.mu.Unlock()
	im.update(func(s *ImportStatus) {
		s.File, s.FileSent, s.FileTotal, s.RateBps = "", 0, 0, 0
		s.Running = false
		s.FinishedAt = time.Now().Format(time.RFC3339)
		if err != nil {
			s.Phase = "error"
			s.Error = err.Error()
		} else {
			s.Phase = "done"
			accepted := upOK + upDup
			suffix := ""
			if upFail > 0 {
				suffix = fmt.Sprintf(", %d failed", upFail)
			}
			switch {
			case staging != "":
				s.Message = fmt.Sprintf("%d files uploaded to Immich%s (no local copy kept)", accepted, suffix)
			case opt.Immich:
				s.Message = fmt.Sprintf("%d files imported to %s, %d uploaded%s", len(files), dest, accepted, suffix)
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
func (im *Importer) copyPhase(app *App, dest string, keepers []keeperFile, totalBytes int64, onFile func(photo.FileEntry)) ([]photo.FileEntry, error) {
	type pullItem struct {
		it   fetchItem
		size int64
		kind string
		idx  int // into files, so a landed pull can be handed straight on
	}
	files := make([]photo.FileEntry, len(keepers))
	var toPull []pullItem
	done := 0

	// Camera-lane counters. Cached files are counted apart from pulled ones and
	// excluded from the rate: they arrive instantly, so folding them in reports
	// a link speed of several GB/s for the first second of every import.
	var (
		cached      int
		bytesDone   int64
		pulledBytes int64
		pullStart   time.Time
	)
	noteCamera := func(complete bool) {
		rate := 0.0
		if !pullStart.IsZero() {
			if el := time.Since(pullStart).Seconds(); el > 0 {
				rate = float64(pulledBytes) / el
			}
		}
		d, c, b := done, cached, bytesDone
		im.update(func(s *ImportStatus) {
			s.Done = d
			s.Camera.Files, s.Camera.Cached = d, c
			s.Camera.Bytes, s.Camera.BytesTotal = b, totalBytes
			s.Camera.Rate = rate
			if complete {
				s.Camera.State = "done"
			}
		})
	}

	for i, k := range keepers {
		name := k.shot.Files[k.ext]
		target := filepath.Join(dest, k.shot.Folder, name)
		files[i] = photo.FileEntry{Folder: k.shot.Folder, Name: name, LocalPath: target}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}

		wantSize := k.shot.Sizes[k.ext]
		if st, err := os.Stat(target); err == nil && st.Size() > 0 && (wantSize == 0 || st.Size() == wantSize) {
			done, cached = done+1, cached+1
			bytesDone += st.Size()
			noteCamera(false)
			onFile(files[i])
			continue
		}
		if cachedPath, ok := app.prefetch.CachedFile(k.shot, k.ext); ok {
			tmp := target + ".tmp"
			if err := copyFile(cachedPath, tmp); err == nil && commit(tmp, target) == nil {
				done, cached = done+1, cached+1
				if sz := wantSize; sz > 0 {
					bytesDone += sz
				} else if st, err := os.Stat(target); err == nil {
					bytesDone += st.Size()
				}
				noteCamera(false)
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
	if len(pending) > 0 {
		pullStart = time.Now()
	}
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
				pulledBytes += st.Size()
				bytesDone += st.Size()
				noteCamera(false)
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
	noteCamera(true)
	return files, nil
}

// sweepStaleStaging removes the per-run temp directories older versions left
// behind. They are unreachable — nothing knows their names after the run that
// made them — so they are pure leaked disk, and an upload-only import of a big
// event leaks several GB of it per failure.
func sweepStaleStaging(cache string) {
	matches, err := filepath.Glob(filepath.Join(cache, "import-stage-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			log.Printf("WARN: removing stale staging dir %s: %v", m, err)
			continue
		}
		log.Printf("import: removed stale staging dir %s", m)
	}
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
