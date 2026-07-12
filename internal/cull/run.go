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

	Dest        string
	ImmichURL   string
	ImmichKey   string
	ImmichAlbum string
	ImmichStack bool
	SkipImmich  bool
	Retries     int
	UploadConc  int
	HashConc    int
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
	switch o.BackendName {
	case "cli":
		if err := mtpcli.Ensure(); err != nil {
			return nil, nil, err
		}
		backend = &cliBackend{roots: strings.Split(o.CameraRoot, ",")}
	case "dir":
		if o.Root == "" {
			return nil, nil, fmt.Errorf("--root is required with --backend dir")
		}
		var dcimRoots []string
		if o.CameraRoot != "" && !strings.Contains(o.CameraRoot, "SLOT") {
			dcimRoots = strings.Split(o.CameraRoot, ",")
		} else {
			var err error
			dcimRoots, err = findDCIMRoots(o.Root)
			if err != nil {
				return nil, nil, err
			}
		}
		log.Printf("DCIM roots under %s: %v", o.Root, dcimRoots)
		backend = &dirBackend{root: o.Root, dcimRoots: dcimRoots}
	default:
		return nil, nil, fmt.Errorf("unknown backend %q (want cli or dir)", o.BackendName)
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
		if prefetch.thumbFetcher != nil || prefetch.partBin != "" {
			go prefetch.localThumbGen()
		}
		app.finishInit(catalog, prefetch)
		if app.imcheck != nil {
			go app.imcheck.Backfill()
		}
		log.Printf("fuji-cull ready: http://%s  (session=%s, backend=%s, %d shots, buffer %d ahead / %d behind)",
			o.Listen, o.SessionName, backend.Name(), len(catalog.Shots), o.Ahead, o.Behind)
	}()

	return app, app.handler(), nil
}

// Close stops background work (prefetcher); safe before readiness.
func (a *App) Close() {
	if a.isReady() {
		a.prefetch.Close()
	}
}
