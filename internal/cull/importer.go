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
	Running    bool   `json:"running"`
	Phase      string `json:"phase"` // idle | copy | hash | upload | validate | done | error
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Message    string `json:"message"`
	Error      string `json:"error"`
	Dest       string `json:"dest"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
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

// keeperFile is one camera file belonging to a kept shot.
type keeperFile struct {
	shot *photo.Shot
	ext  string
}

// Start kicks off an import of the current keepers in the background.
func (im *Importer) Start(app *App, dest, album string) error {
	if dest == "" {
		return fmt.Errorf("no destination configured; pass --dest at startup or in the import request")
	}
	keepers := app.keeperFiles()
	if len(keepers) == 0 {
		return fmt.Errorf("no shots marked as keep")
	}

	im.mu.Lock()
	if im.status.Running {
		im.mu.Unlock()
		return fmt.Errorf("an import is already running")
	}
	saveImportDefaults(dest, album) // prefill the panel next session
	im.status = ImportStatus{
		Running:   true,
		Phase:     "copy",
		Total:     len(keepers),
		Dest:      dest,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	im.mu.Unlock()

	go im.run(app, dest, album, keepers)
	return nil
}

func (im *Importer) run(app *App, dest, album string, keepers []keeperFile) {
	// The camera link is single-threaded: wait out any in-flight prefetch,
	// then own the link for the whole copy phase.
	app.prefetch.PauseAndDrain()
	defer app.prefetch.Resume()

	files, err := im.copyPhase(app, dest, keepers)
	if err == nil {
		opts := app.pipelineOpts
		opts.Dest = dest
		opts.ImmichAlbum = album
		opts.Progress = func(phase string, done, total int) {
			im.update(func(s *ImportStatus) { s.Phase = phase; s.Done = done; s.Total = total })
		}
		im.update(func(s *ImportStatus) { s.Phase = "hash"; s.Done = 0; s.Total = len(files) })
		err = pipeline.Run(context.Background(), opts, files)
	}

	im.update(func(s *ImportStatus) {
		s.Running = false
		s.FinishedAt = time.Now().Format(time.RFC3339)
		if err != nil {
			s.Phase = "error"
			s.Error = err.Error()
		} else {
			s.Phase = "done"
			s.Message = fmt.Sprintf("%d files imported to %s", len(files), dest)
		}
	})
	if err != nil {
		log.Printf("import failed: %v", err)
	} else {
		log.Printf("import complete: %d files -> %s", len(files), dest)
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
func (im *Importer) copyPhase(app *App, dest string, keepers []keeperFile) ([]photo.FileEntry, error) {
	type pullItem struct {
		it   fetchItem
		size int64
		kind string
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
			continue
		}
		if cached, ok := app.prefetch.CachedFile(k.shot, k.ext); ok {
			tmp := target + ".tmp"
			if err := copyFile(cached, tmp); err == nil && commit(tmp, target) == nil {
				done++
				im.update(func(s *ImportStatus) { s.Done = done })
				continue
			}
			os.Remove(tmp)
		}
		kind := "jpg"
		switch {
		case k.ext == "RAF":
			kind = "raf"
		case k.shot.Kind == "video":
			kind = "mov"
		}
		toPull = append(toPull, pullItem{
			it: fetchItem{
				CameraDir: k.shot.CameraDir, Name: name,
				ObjectID: k.shot.ObjectIDs[k.ext], Dest: target + ".tmp",
			},
			size: wantSize, kind: kind,
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
			fetchErr := app.backend.Fetch(ctx, items)
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
func (a *App) keeperFiles() []keeperFile {
	decisions := a.session.Decisions()
	var out []keeperFile
	for _, s := range a.catalog.Shots {
		if decisions[s.ID] != "keep" {
			continue
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
	return out
}
