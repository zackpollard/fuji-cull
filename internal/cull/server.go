package cull

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/pipeline"
)

//go:embed ui
var uiFS embed.FS

// App wires the catalog, session, prefetcher and importer behind the HTTP API.
// The HTTP server binds before camera discovery finishes: until ready, only
// /api/state answers (with live discovery progress) and everything else 503s.
type App struct {
	backend      Backend
	catalog      *Catalog
	session      *Session
	prefetch     *Prefetcher
	importer     *Importer
	imcheck      *immichChecker
	pipelineOpts pipeline.Options
	sessionName  string
	dest         string
	album        string

	mu        sync.RWMutex
	ready     bool
	discStage string
	discFiles int
	discErr   string
}

func (a *App) setDiscovery(stage string, files int) {
	a.mu.Lock()
	a.discStage, a.discFiles = stage, files
	a.discErr = "" // a new attempt is making progress; clear the stale error
	a.mu.Unlock()
}

func (a *App) setDiscoveryError(err error) {
	a.mu.Lock()
	a.discErr = err.Error()
	a.mu.Unlock()
}

// finishInit publishes the fully-initialized catalog/prefetcher to handlers.
func (a *App) finishInit(cat *Catalog, pf *Prefetcher) {
	a.mu.Lock()
	a.catalog = cat
	a.prefetch = pf
	a.ready = true
	a.mu.Unlock()
}

func (a *App) isReady() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready
}

// videoDirect reports whether videos can stream straight off the backend
// (dir backend) or must be pulled to cache first (cli backend).
func (a *App) videoDirect() bool {
	_, ok := a.backend.(*dirBackend)
	return ok
}

type shotDTO struct {
	ID     string            `json:"id"`
	Folder string            `json:"folder"`
	Base   string            `json:"base"`
	Kind   string            `json:"kind"`
	Files  map[string]string `json:"files"`
	Size   int64             `json:"size"`
}

func (a *App) counts() map[string]int {
	d := a.session.Decisions()
	c := map[string]int{"keep": 0, "reject": 0, "undecided": 0}
	for _, s := range a.catalog.Shots {
		switch d[s.ID] {
		case "keep":
			c["keep"]++
		case "reject":
			c["reject"]++
		default:
			c["undecided"]++
		}
	}
	return c
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()

	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(sub))

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		if !a.isReady() {
			a.mu.RLock()
			resp := map[string]any{
				"discovering": true,
				"stage":       a.discStage,
				"files":       a.discFiles,
				"error":       a.discErr,
			}
			a.mu.RUnlock()
			writeJSON(w, resp)
			return
		}
		shots := make([]shotDTO, len(a.catalog.Shots))
		for i, s := range a.catalog.Shots {
			shots[i] = shotDTO{
				ID: s.ID, Folder: s.Folder, Base: s.Base, Kind: s.Kind,
				Files: s.Files, Size: s.TotalSize(),
			}
		}
		writeJSON(w, map[string]any{
			"session":     a.sessionName,
			"backend":     a.backend.Name(),
			"videoDirect": a.videoDirect(),
			"dest":        a.dest,
			"album":       a.album,
			"cursor":      a.session.Cursor(),
			"shots":       shots,
			"decisions":   a.session.Decisions(),
			"counts":      a.counts(),
			"import":      a.importer.Status(),
		})
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		bulkSick, partSick := a.prefetch.LinkSick()
		writeJSON(w, map[string]any{
			"cursor":    a.session.Cursor(),
			"decisions": a.session.Decisions(),
			"fetch":     a.prefetch.Snapshot(),
			"counts":    a.counts(),
			"import":    a.importer.Status(),
			"bulkSick":  bulkSick,
			"partSick":  partSick,
			"streaming": a.prefetch.StreamingAvailable() && !a.importer.Status().Running,
		})
	})

	mux.HandleFunc("GET /api/image", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		path, err := a.prefetch.Wait(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// The bytes for a given shot never change: let the browser cache them
		// so re-visiting a shot works even after the server evicts its copy.
		w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, path)
	})

	mux.HandleFunc("GET /api/video", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		s := a.catalog.Get(id)
		if s == nil {
			http.Error(w, "unknown shot", http.StatusNotFound)
			return
		}
		var name, ext string
		for _, e := range []string{"MOV", "MP4"} {
			if n, ok := s.Files[e]; ok {
				name, ext = n, e
				break
			}
		}
		if name == "" {
			http.Error(w, "shot has no video file", http.StatusNotFound)
			return
		}
		// Local copies win (direct backend path or the pulled cache copy);
		// otherwise stream ranges straight off the camera via the persistent
		// partial-read session. Explicit /api/loadvideo still pulls a full
		// local copy for smoother scrubbing and import reuse.
		path, ok := a.backend.LocalPath(s, ext)
		if !ok {
			path, ok = a.prefetch.CachedFile(s, ext)
		}
		if !ok {
			if a.CanStreamVideo(id) {
				rs, err := a.prefetch.StreamReader(s, ext)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadGateway)
					return
				}
				http.ServeContent(w, r, name, time.Time{}, rs)
				return
			}
			http.Error(w, "video not buffered; POST /api/loadvideo first", http.StatusConflict)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, name, time.Time{}, f) // Range support for seeking
	})

	mux.HandleFunc("GET /api/thumb", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		s := a.catalog.Get(id)
		if s == nil || !a.prefetch.HasThumb(s) {
			http.Error(w, "no thumbnail", http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
		w.Header().Set("Content-Type", "image/jpeg")
		// Thumb files stay in sensor orientation; rotate at delivery once the
		// shot's orientation is known. Clients re-request with an &o= cache
		// buster when orientation data arrives after the first fetch.
		if or := a.prefetch.OrientOf(id); or > 1 {
			if data, err := rotatedThumbJPEG(a.prefetch.ThumbPath(s), or); err == nil {
				w.Write(data)
				return
			}
		}
		http.ServeFile(w, r, a.prefetch.ThumbPath(s))
	})

	mux.HandleFunc("GET /api/thumbs", func(w http.ResponseWriter, r *http.Request) {
		states, have := a.prefetch.ThumbStates()
		writeJSON(w, map[string]any{"states": states, "have": have,
			"orient": a.prefetch.OrientStates(), "immich": a.ImmichStates()})
	})

	mux.HandleFunc("POST /api/thumbhint", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Index int `json:"index"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Index >= 0 && req.Index < len(a.catalog.Shots) {
			a.prefetch.SetThumbHint(req.Index)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/loadvideo", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s := a.catalog.Get(req.ID)
		if s == nil || s.Kind != "video" {
			http.Error(w, "unknown video shot", http.StatusNotFound)
			return
		}
		a.EnsureVideo(req.ID) // releases any live stream holding the link
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("POST /api/decision", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID       string `json:"id"`
			Decision string `json:"decision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Decision == "clear" {
			req.Decision = ""
		}
		if req.Decision != "" && req.Decision != "keep" && req.Decision != "reject" {
			http.Error(w, "decision must be keep, reject or clear", http.StatusBadRequest)
			return
		}
		if a.catalog.Get(req.ID) == nil {
			http.Error(w, "unknown shot", http.StatusNotFound)
			return
		}
		if err := a.session.SetDecision(req.ID, req.Decision); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"counts": a.counts()})
	})

	mux.HandleFunc("POST /api/cursor", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Index int `json:"index"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Index < 0 || req.Index >= len(a.catalog.Shots) {
			http.Error(w, "index out of range", http.StatusBadRequest)
			return
		}
		if err := a.session.SetCursor(req.Index); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a.prefetch.SetCursor(req.Index)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/retry", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.prefetch.Retry(req.ID)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/import", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dest  string `json:"dest"`
			Album string `json:"album"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dest, album := a.dest, a.album
		if req.Dest != "" {
			dest = req.Dest
		}
		if req.Album != "" {
			album = req.Album
		}
		if err := a.importer.Start(a, dest, album); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		log.Printf("import started: dest=%s album=%q", dest, album)
		writeJSON(w, a.importer.Status())
	})

	// Until discovery finishes, only the UI itself and /api/state (which
	// reports discovery progress) are served; other API calls 503.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.isReady() && strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/state" {
			http.Error(w, "still discovering camera contents", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("WARN: encode response: %v", err)
	}
}
