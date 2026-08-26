package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/hashutil"
	"github.com/zack/fuji-tools/internal/immich"
	"github.com/zack/fuji-tools/internal/photo"
)

// Streamer overlaps the camera copy with hashing and uploading.
//
// Run() is strictly phased: every file is pulled off the camera, then every
// file is hashed, then every file is uploaded. Those phases use different
// resources — the camera link is USB-bound and single-threaded, hashing is CPU,
// uploading is network — so running them one after another leaves two of the
// three idle at any moment and makes an import take about as long as the sum of
// its parts. Feeding each file into hash+upload the moment it lands means the
// upload of shot N overlaps the pull of shot N+1, and the import costs roughly
// the slowest single stage instead.
//
// Validation still happens once at the end, against everything: it is a bulk
// checksum query, and it is what licenses deleting a local copy in upload-only
// mode.
type Streamer struct {
	ctx     context.Context
	opts    Options
	client  *immich.Client
	albumID string
	total   int

	jobs chan *photo.FileEntry
	// sem bounds files on disk ahead of the uploader: Add blocks once the
	// buffer is full, which throttles the camera pull rather than letting a
	// staged import grow to the size of the whole selection.
	sem chan struct{}
	// Bytes outstanding alongside the file count, so one big video cannot
	// blow the footprint the count was supposed to bound.
	bytesCond *sync.Cond
	outBytes  int64
	maxBytes  int64
	wg        sync.WaitGroup

	mu      sync.Mutex
	bytesMu sync.Mutex
	// in-flight upload progress, keyed by file; several run at once, so the
	// one reported is the biggest — that is the one a person is waiting on.
	progMu   sync.Mutex
	inflight map[string]*fileProg
	rateAt   time.Time
	rateSent int64
	rateBps  float64
	// Entries are pointers so the workers' references stay valid as more
	// files arrive; appending to a slice of values would move them.
	entries        []*photo.FileEntry
	dirs           map[string]struct{}
	ok, dup, fail  int
	upBytes        int64
	upDone         bool
	toAlbum        []string
	failedMessages []string
	errs           []string
}

// NewStreamer resolves the album (once, up front) and starts the workers.
// `total` is only used for progress reporting.
func NewStreamer(ctx context.Context, opts Options, total int) (*Streamer, error) {
	s := &Streamer{
		ctx:      ctx,
		opts:     opts,
		total:    total,
		jobs:     make(chan *photo.FileEntry, 64),
		dirs:     map[string]struct{}{},
		inflight: map[string]*fileProg{},
	}
	if opts.BufferAhead > 0 {
		s.sem = make(chan struct{}, opts.BufferAhead)
	}
	if opts.BufferBytes > 0 {
		s.maxBytes = opts.BufferBytes
		s.bytesCond = sync.NewCond(&s.bytesMu)
	}
	if !opts.SkipImmich {
		s.client = immich.NewClient(opts.ImmichURL, opts.ImmichKey)
		if opts.FileProgress != nil {
			s.client.OnProgress = s.noteFileProgress
		}
		if opts.ImmichAlbum != "" {
			id, err := s.client.EnsureAlbum(ctx, opts.ImmichAlbum)
			if err != nil {
				return nil, fmt.Errorf("ensure album %q: %w", opts.ImmichAlbum, err)
			}
			s.albumID = id
			log.Printf("Album %q -> id=%s", opts.ImmichAlbum, id)
		}
	}
	n := opts.UploadConcurrency
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s, nil
}

// Add hands a freshly copied file to the pipeline. It returns as soon as the
// file is queued, so the caller can get back to the camera.
func (s *Streamer) Add(f photo.FileEntry) {
	// Blocks while the buffer is full — this is the backpressure that bounds
	// disk use in upload-only mode.
	if s.sem != nil {
		s.sem <- struct{}{}
	}
	// Size is not known until the file is on disk, which it now is.
	if st, err := os.Stat(f.LocalPath); err == nil {
		f.Size = st.Size()
	}
	s.acquireBytes(f.Size)
	e := &f
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.dirs[filepath.Dir(f.LocalPath)] = struct{}{}
	s.mu.Unlock()
	s.jobs <- e
}

func (s *Streamer) worker() {
	defer s.wg.Done()
	for f := range s.jobs {
		size := f.Size
		s.handle(f)
		s.releaseBytes(size)
		if s.sem != nil {
			<-s.sem // free a buffer slot so the camera pull can continue
		}
	}
}

// acquireBytes admits a file whenever the buffer is currently under budget,
// even if that file takes it over.
//
// The stricter rule — refuse anything that would exceed the budget — stalls
// the camera on exactly the case that matters: a 10 GB video queued behind 40
// photos would wait for every photo to finish uploading before it could even
// start copying, leaving the link idle. Overshooting by one file costs a bit
// of transient disk and keeps the pull moving. It also means an oversized file
// can never deadlock: at an empty buffer, outBytes is 0 and anything gets in.
func (s *Streamer) acquireBytes(n int64) {
	if s.bytesCond == nil {
		return
	}
	s.bytesMu.Lock()
	for s.outBytes >= s.maxBytes {
		s.bytesCond.Wait()
	}
	s.outBytes += n
	s.bytesMu.Unlock()
}

func (s *Streamer) releaseBytes(n int64) {
	if s.bytesCond == nil {
		return
	}
	s.bytesMu.Lock()
	s.outBytes -= n
	s.bytesCond.Broadcast()
	s.bytesMu.Unlock()
}

func (s *Streamer) handle(f *photo.FileEntry) {
	if s.ctx.Err() != nil {
		return
	}
	st, err := os.Stat(f.LocalPath)
	if err != nil {
		s.note(fmt.Sprintf("stat %s: %v", f.LocalPath, err))
		return
	}
	f.Size = st.Size()

	// Hashing only earns its keep when something will verify against it.
	if s.opts.SkipImmich {
		return
	}
	sum, b64, err := hashutil.SHA1File(f.LocalPath)
	if err != nil {
		s.note(fmt.Sprintf("hash %s: %v", f.LocalPath, err))
		return
	}
	f.SHA1, f.SHA1B64 = sum, b64
	if s.upload(f) && s.opts.DeleteAfterUpload {
		// Accepted by the server, and the bytes were checksum-verified on the
		// way in. Dropping the staged copy now is what keeps the footprint
		// flat; the camera still has the original either way.
		if err := os.Remove(f.LocalPath); err != nil {
			log.Printf("WARN: remove staged %s: %v", f.LocalPath, err)
		}
	}
}

// upload returns true when the server accepted the file.
func (s *Streamer) upload(f *photo.FileEntry) bool {
	if s.opts.DryRun {
		return false
	}
	assetID, duplicate, err := s.client.Upload(s.ctx, f)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil:
		s.fail++
		s.failedMessages = append(s.failedMessages, fmt.Sprintf("%s: %v", f.LocalPath, err))
	case duplicate:
		s.dup++
		s.upBytes += f.Size
		f.AssetID = assetID
		if s.albumID != "" && assetID != "" {
			s.toAlbum = append(s.toAlbum, assetID)
		}
	default:
		s.ok++
		s.upBytes += f.Size
		f.AssetID = assetID
		if s.albumID != "" {
			s.toAlbum = append(s.toAlbum, assetID)
		}
	}
	// Accepted files only. Counting failures as progress let a run where every
	// upload failed finish with a full bar.
	s.opts.progress("upload", s.ok+s.dup, s.total)
	s.emitUpload()
	return err == nil
}

// emitUpload publishes the upload stage. Caller holds s.mu.
func (s *Streamer) emitUpload() {
	s.progMu.Lock()
	bps := s.rateBps
	s.progMu.Unlock()
	s.opts.stage(StageUpload, Stage{
		Files:      s.ok + s.dup,
		FilesTotal: s.total,
		Bytes:      s.upBytes,
		BytesTotal: s.opts.TotalBytes,
		Rate:       bps,
		Failed:     s.fail,
		Done:       s.upDone,
	})
}

// fileProg is one upload's byte counter.
type fileProg struct {
	sent, total int64
}

// noteFileProgress records an upload's progress and publishes whichever
// in-flight file is largest, with a rate smoothed over ~1s. Uploads are
// concurrent, so reporting "the current file" needs a rule: biggest wins,
// because that is the one the person is actually waiting for.
func (s *Streamer) noteFileProgress(name string, sent, total int64) {
	s.progMu.Lock()
	fp := s.inflight[name]
	if fp == nil {
		fp = &fileProg{}
		s.inflight[name] = fp
	}
	delta := sent - fp.sent
	fp.sent, fp.total = sent, total
	if sent >= total {
		delete(s.inflight, name)
	}

	// rate: bytes across all uploads over the last sample window
	s.rateSent += delta
	now := time.Now()
	if s.rateAt.IsZero() {
		s.rateAt = now
	}
	if d := now.Sub(s.rateAt); d >= time.Second {
		inst := float64(s.rateSent) / d.Seconds()
		if s.rateBps == 0 {
			s.rateBps = inst
		} else {
			s.rateBps = 0.6*s.rateBps + 0.4*inst // smooth the sawtooth
		}
		s.rateAt, s.rateSent = now, 0
	}

	// pick the biggest in-flight file to display
	var bigName string
	var big *fileProg
	for n, f := range s.inflight {
		if big == nil || f.total > big.total {
			bigName, big = n, f
		}
	}
	bps := s.rateBps
	s.progMu.Unlock()

	if big != nil {
		s.opts.FileProgress(bigName, big.sent, big.total, bps)
	} else {
		s.opts.FileProgress("", 0, 0, bps)
	}
}

func (s *Streamer) note(msg string) {
	s.mu.Lock()
	s.errs = append(s.errs, msg)
	s.mu.Unlock()
}

// Counts reports what the server actually did with the files: accepted as new,
// recognised as duplicates, and failed. Callers report outcomes with these
// rather than with len(files), which is only ever the number ATTEMPTED — a
// partly-failed run otherwise announces itself as a complete one.
func (s *Streamer) Counts() (ok, dup, fail int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ok, s.dup, s.fail
}

// Files returns the entries as values, with whatever the workers learned.
func (s *Streamer) Files() []photo.FileEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]photo.FileEntry, len(s.entries))
	for i, e := range s.entries {
		out[i] = *e
	}
	return out
}

// Wait closes the queue, drains the workers, then finishes the run: album
// membership, mtime restamping, validation with retries, and stacking. The
// returned error is non-nil only when files are genuinely not on the server —
// which is what callers must not ignore before deleting a local copy.
func (s *Streamer) Wait() error {
	close(s.jobs)
	s.wg.Wait()

	s.mu.Lock()
	errs := append([]string(nil), s.errs...)
	dirs := make([]string, 0, len(s.dirs))
	for d := range s.dirs {
		dirs = append(dirs, d)
	}
	ok, dup, fail := s.ok, s.dup, s.fail
	failed := append([]string(nil), s.failedMessages...)
	toAlbum := append([]string(nil), s.toAlbum...)
	s.mu.Unlock()

	if len(errs) > 0 {
		return fmt.Errorf("%d file error(s): %s", len(errs), errs[0])
	}

	// mtimes are for the local archive, not the upload, so one pass per
	// directory at the end beats restamping per file.
	if !s.opts.DryRun {
		for _, d := range dirs {
			if err := exif.RestampMtime(d); err != nil {
				log.Printf("WARN: restamp %s: %v", d, err)
			}
		}
	}

	files := s.Files()
	if s.opts.SkipImmich {
		Report(s.opts.Dest, files)
		return nil
	}

	log.Printf("Upload summary: uploaded=%d duplicate=%d failed=%d", ok, dup, fail)
	for _, m := range failed {
		log.Printf("  FAIL: %s", m)
	}
	if s.albumID != "" && len(toAlbum) > 0 {
		seen := map[string]struct{}{}
		uniq := make([]string, 0, len(toAlbum))
		for _, id := range toAlbum {
			if _, dupe := seen[id]; dupe {
				continue
			}
			seen[id] = struct{}{}
			uniq = append(uniq, id)
		}
		log.Printf("Adding %d assets to album", len(uniq))
		if err := s.client.AddToAlbum(s.ctx, s.albumID, uniq); err != nil {
			log.Printf("WARN: add to album failed: %v", err)
		}
	}

	log.Printf("--- validating against Immich ---")
	s.opts.progress("validate", 0, len(files))
	s.opts.stage(StageVerify, Stage{FilesTotal: len(files)})
	missing, err := Validate(s.ctx, s.client, files)
	if err != nil {
		return fmt.Errorf("validate phase: %w", err)
	}
	s.opts.stage(StageVerify, Stage{
		Files: len(files) - len(missing), FilesTotal: len(files), Done: len(missing) == 0,
	})
	// Retries report against the streamer's own denominators. The package-level
	// Upload counts against len(missing), so letting it drive progress made the
	// upload counter COLLAPSE from 400/400 to 2/2 the moment a retry began.
	retryOpts := s.opts
	retryOpts.Progress, retryOpts.StageProgress, retryOpts.FileProgress = nil, nil, nil
	for attempt := 1; attempt <= s.opts.Retries && len(missing) > 0; attempt++ {
		log.Printf("--- retry %d/%d for %d missing file(s) ---", attempt, s.opts.Retries, len(missing))
		var wasMissing int64
		for _, f := range missing {
			wasMissing += f.Size
		}
		if err := Upload(s.ctx, retryOpts, s.client, s.albumID, missing); err != nil {
			log.Printf("retry upload error: %v", err)
		}
		missing, err = Validate(s.ctx, s.client, files)
		if err != nil {
			return fmt.Errorf("revalidation: %w", err)
		}
		// A retry that landed is real progress on both stages: the file is on
		// the server, so it is no longer a failure. Its BYTES have to move too
		// — the retry runs through the package-level Upload, which never
		// touches upBytes, so the lane finished a clean import reading 95%.
		var stillMissing int64
		for _, f := range missing {
			stillMissing += f.Size
		}
		s.mu.Lock()
		if s.fail > len(missing) {
			s.ok += s.fail - len(missing)
			s.fail = len(missing)
		}
		if d := wasMissing - stillMissing; d > 0 {
			s.upBytes += d
		}
		s.emitUpload()
		s.mu.Unlock()
		s.opts.stage(StageVerify, Stage{
			Files: len(files) - len(missing), FilesTotal: len(files), Done: len(missing) == 0,
		})
	}
	// Uploading is over once the retries have settled — pass or fail. Without
	// this the lane sat at "running" on a finished import, so every client drew
	// the upload as still in flight after the run ended.
	s.mu.Lock()
	s.upDone = true
	s.emitUpload()
	s.mu.Unlock()

	if len(missing) > 0 {
		for _, f := range missing {
			log.Printf("  MISSING: %s", f.LocalPath)
		}
		Report(s.opts.Dest, files)
		return fmt.Errorf("%d file(s) missing in Immich after %d retries", len(missing), s.opts.Retries)
	}
	log.Printf("All %d files verified in Immich", len(files))

	if s.opts.ImmichStack && !s.opts.DryRun {
		log.Printf("--- stacking RAF+JPG pairs ---")
		StackPairs(s.ctx, s.opts, s.client, files)
	}
	Report(s.opts.Dest, files)
	return nil
}
