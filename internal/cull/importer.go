package cull

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	}
}

// copyPhase lands keeper files in dest/<folder>/: files already present are
// kept, cached camera-verbatim copies (prefetched JPGs/RAFs/videos) are
// copied locally, and the remainder is pulled from the camera in per-folder
// batches. Returns the FileEntry list for the pipeline.
func (im *Importer) copyPhase(app *App, dest string, keepers []keeperFile) ([]photo.FileEntry, error) {
	files := make([]photo.FileEntry, len(keepers))
	var toPull []fetchItem
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
		toPull = append(toPull, fetchItem{
			CameraDir: k.shot.CameraDir, Name: name,
			ObjectID: k.shot.ObjectIDs[k.ext], Dest: target + ".tmp",
		})
	}

	if len(toPull) > 0 {
		log.Printf("import: pulling %d files from camera (%d satisfied locally)", len(toPull), done)
		fetchErr := app.backend.Fetch(context.Background(), toPull)
		// Promote whatever landed so a re-run skips it, even on batch error.
		for _, it := range toPull {
			target := it.Dest[:len(it.Dest)-len(".tmp")]
			st, err := os.Stat(it.Dest)
			if err != nil || st.Size() == 0 {
				os.Remove(it.Dest)
				if fetchErr == nil {
					fetchErr = fmt.Errorf("pull produced no data for %s/%s", it.CameraDir, it.Name)
				}
				continue
			}
			if err := commit(it.Dest, target); err != nil {
				return nil, err
			}
			done++
			im.update(func(s *ImportStatus) { s.Done = done })
		}
		if fetchErr != nil {
			return nil, fmt.Errorf("camera pull: %w", fetchErr)
		}
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
