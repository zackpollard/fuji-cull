// Package cull is the shared core of fuji-cull: camera backends, prefetch
// buffer, thumbnail sweep, sessions, importer, and the HTTP API + web UI.
// Frontends (the web server binary and the native GUI) both run through it.
package cull

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/immich"
	"github.com/zack/fuji-tools/internal/mtpcli"
	"github.com/zack/fuji-tools/internal/pipeline"
)

// Options configures a cull run (flag parsing lives in the frontends).
type Options struct {
	Listen      string
	BackendName string
	Root        string
	CameraRoot  string
	SessionName string
	CacheDir    string
	Ahead       int
	Behind      int
	EvictMargin int
	Batch       int

	// Transport, when set, drives the camera through an injected link instead
	// of exec'ing aft — the iOS ImageCaptureCore path (BackendName ignored).
	Transport Transport

	Dest        string
	ImmichURL   string
	ImmichKey   string
	ImmichAlbum string
	ImmichStack bool
	SkipImmich  bool
	Retries     int
	UploadConc  int
	HashConc    int

	// Cross-device progress sync (optional). When both are set, the engine
	// pushes decisions to / pulls them from a self-hosted fuji-sync server.
	SyncURL string
	SyncKey string
}

// Start wires the app, kicks off background discovery, and returns the App
// plus the ready-to-serve HTTP handler. It does not listen; frontends do.
func Start(o Options) (*App, http.Handler, error) {
	if err := exif.EnsurePath(); err != nil {
		return nil, nil, err
	}
	if !o.SkipImmich && (o.ImmichURL == "" || o.ImmichKey == "") {
		log.Printf("WARN: Immich URL/key not configured; imports will only copy to the destination (--skip-immich implied)")
		o.SkipImmich = true
	}

	var backend Backend
	switch {
	case o.Transport != nil:
		backend = &iccBackend{t: o.Transport}
	default:
		var err error
		backend, err = pickBackend(o)
		if err != nil {
			return nil, nil, err
		}
	}

	home, _ := os.UserHomeDir()
	sessPath := filepath.Join(home, ".local", "share", "fuji-cull", "sessions", o.SessionName+".json")
	session, err := loadSession(sessPath)
	if err != nil {
		return nil, nil, fmt.Errorf("session: %w", err)
	}
	cache := o.CacheDir
	if cache == "" {
		cache = filepath.Join(home, ".cache", "fuji-cull", o.SessionName)
	}
	if cb, ok := backend.(*cliBackend); ok {
		cb.cacheDir = cache // catalog cache lives with the image buffer
	}
	return startWith(o, backend, session, cache)
}

// pickBackend builds the exec-based backends (desktop/Android).
func pickBackend(o Options) (Backend, error) {
	switch o.BackendName {
	case "cli":
		if err := mtpcli.Ensure(); err != nil {
			return nil, err
		}
		return &cliBackend{roots: strings.Split(o.CameraRoot, ",")}, nil
	case "dir":
		if o.Root == "" {
			return nil, fmt.Errorf("--root is required with --backend dir")
		}
		var dcimRoots []string
		if o.CameraRoot != "" && !strings.Contains(o.CameraRoot, "SLOT") {
			dcimRoots = strings.Split(o.CameraRoot, ",")
		} else {
			var err error
			dcimRoots, err = findDCIMRoots(o.Root)
			if err != nil {
				return nil, err
			}
		}
		log.Printf("DCIM roots under %s: %v", o.Root, dcimRoots)
		return &dirBackend{root: o.Root, dcimRoots: dcimRoots}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want cli, dir, or an injected Transport)", o.BackendName)
	}
}

// startWith builds the app around an already-chosen backend and kicks off
// background discovery.
func startWith(o Options, backend Backend, session *Session, cache string) (*App, http.Handler, error) {
	// Flags win; otherwise prefill from whatever the last import ran with.
	remembered := loadImportDefaults()
	if o.Dest == "" {
		o.Dest = remembered.Dest
	}
	if o.ImmichAlbum == "" {
		o.ImmichAlbum = remembered.Album
	}

	app := &App{
		backend:     backend,
		session:     session,
		importer:    &Importer{},
		sessionName: o.SessionName,
		dest:        o.Dest,
		album:       o.ImmichAlbum,
		pipelineOpts: pipeline.Options{
			ImmichURL:         strings.TrimRight(o.ImmichURL, "/"),
			ImmichKey:         o.ImmichKey,
			ImmichStack:       o.ImmichStack,
			SkipImmich:        o.SkipImmich,
			Retries:           o.Retries,
			UploadConcurrency: o.UploadConc,
			HashConcurrency:   o.HashConc,
		},
	}

	// Discovery runs in the background so the UI is reachable immediately;
	// /api/state reports progress until finishInit flips the app ready.
	// Failures retry forever — the camera may be plugged in, powered on or
	// power-cycled long after the app starts.
	go func() {
		var catalog *Catalog
		for {
			log.Printf("discovering camera files (backend=%s)...", backend.Name())
			listings, err := backend.Discover(context.Background(), app.setDiscovery)
			if err == nil {
				catalog = buildCatalog(listings)
				if len(catalog.Shots) > 0 {
					break
				}
				err = fmt.Errorf("no shots found on camera")
			}
			log.Printf("discover: %v (retrying in 5s)", err)
			app.setDiscoveryError(err)
			time.Sleep(5 * time.Second)
		}
		log.Printf("catalog: %d shots", len(catalog.Shots))
		// Re-key the session to the camera's identity once discovery has it:
		// name-only sessions let two cards with overlapping DSCF numbering
		// bleed decisions into each other. An explicit --session still wins
		// (desktop power use); mobile passes none and gets per-camera keying.
		if o.SessionName == "" || o.SessionName == "default" {
			if ib, ok := backend.(interface{ CameraIdentity() string }); ok {
				if id := ib.CameraIdentity(); id != "" {
					slug := slugify(id)
					keyed, err := loadSession(filepath.Join(filepath.Dir(session.path), slug+".json"))
					if err != nil {
						log.Printf("session: per-camera load failed (%v) — staying on %q", err, o.SessionName)
					} else {
						keyed.SetCameraSlug(slug) // persist so sync can run camera-absent later
						app.mu.Lock()
						app.session = keyed
						app.camera = id
						app.cameraSlug = slug
						app.mu.Unlock()
						session = keyed
						log.Printf("session: keyed to camera %q", id)
					}
					// The CACHE must scope with the camera too: thumbs and
					// orientation are keyed by shot ID (SLOT 1/DCIM/...),
					// and the fake corpus mirrors real folder layouts — a
					// force-fake run wrote synthetic thumbnails that then
					// served for REAL shots with colliding IDs.
					// migration cleanup: the pre-scoping flat cache may hold
					// another source's thumbnails under colliding IDs (the
					// fake-corpus gradients did exactly this); it's derived
					// data, so reclaim it rather than leave a poisoned GB
					for _, stale := range []string{
						filepath.Join(cache, "thumbs"),
						filepath.Join(cache, "orientation.json"),
					} {
						if _, err := os.Stat(stale); err == nil {
							os.RemoveAll(stale)
							log.Printf("cache: removed legacy unscoped %s", stale)
						}
					}
					cache = filepath.Join(cache, slug)
					log.Printf("cache: scoped to %s", cache)
				}
			}
		}
		if _, ok := backend.(*dirBackend); ok {
			// local trees (fake corpus, --backend dir) get their own cache
			// namespace so they can never pollute a camera's
			cache = filepath.Join(cache, "local")
		}
		// Explicit sync-namespace override: pin the sync slug without disturbing
		// session/cache keying. Lets a --backend dir tree (a NAS folder, the fake
		// corpus) sync, and is the escape hatch when a body reports no serial.
		app.mu.RLock()
		haveSlug := app.cameraSlug != ""
		app.mu.RUnlock()
		if !haveSlug {
			if v := strings.TrimSpace(os.Getenv("FUJI_SYNC_CAMERA")); v != "" {
				slug := slugify(v)
				app.mu.Lock()
				app.cameraSlug = slug
				app.mu.Unlock()
				session.SetCameraSlug(slug)
			}
		}
		cursor := session.Cursor()
		if cursor < 0 || cursor >= len(catalog.Shots) {
			cursor = 0
			_ = session.SetCursor(0)
		}
		prefetch, err := newPrefetcher(catalog, backend, cache, o.Ahead, o.Behind, o.EvictMargin, o.Batch, cursor)
		if err != nil {
			log.Printf("prefetch: %v", err)
			app.setDiscoveryError(err)
			return
		}
		// Immich presence: hash camera-verbatim files as they land and
		// bulk-check them so the UIs can badge already-uploaded shots.
		if !o.SkipImmich && o.ImmichURL != "" && o.ImmichKey != "" {
			client := immich.NewClient(strings.TrimRight(o.ImmichURL, "/"), o.ImmichKey)
			app.imcheck = newImmichChecker(client, prefetch, catalog, cache)
			prefetch.onReady = app.imcheck.Enqueue
		}
		go prefetch.Run()
		if prefetch.thumbFetcher != nil || prefetch.partsOK() {
			go prefetch.localThumbGen()
		}
		if prefetch.localThumbs {
			go prefetch.localThumbSweep()
		}
		app.finishInit(catalog, prefetch)
		if app.imcheck != nil {
			go app.imcheck.Backfill()
		}
		// Cross-device sync: push decisions to / pull them from the self-hosted
		// server. Optional (built only when configured), offline-tolerant, and
		// woken by every local decision. It reads the live session pointer via
		// app.syncTarget() so a re-key never strands it on the old file.
		if o.SyncURL != "" && o.SyncKey != "" {
			app.mu.Lock()
			sy := newSyncer(newSyncClient(o.SyncURL, o.SyncKey), app.syncTarget)
			app.sync = sy
			sess := app.session
			app.mu.Unlock()
			if sess != nil {
				sess.SetOnDirty(sy.Nudge)
			}
			go sy.Run()
			log.Printf("sync: enabled -> %s", o.SyncURL)
		}
		log.Printf("fuji-cull ready: http://%s  (session=%s, backend=%s, %d shots, buffer %d ahead / %d behind)",
			o.Listen, o.SessionName, backend.Name(), len(catalog.Shots), o.Ahead, o.Behind)
	}()

	return app, app.handler(), nil
}

// Close stops background work (prefetcher, syncer); safe before readiness.
// Correctness never depends on Close (the desktop GUI never calls it) — the
// syncer's outbox is already durable on disk; this is a best-effort clean stop.
func (a *App) Close() {
	a.mu.RLock()
	sy := a.sync
	a.mu.RUnlock()
	if sy != nil {
		sy.Stop()
	}
	if a.isReady() {
		a.prefetch.Close()
	}
}
