// fuji-cull: cull Fuji camera photos in a web UI straight off the camera.
//
// Photos stay on the camera while you review; a sliding window of full-size
// previews is buffered to local disk so browsing is instant. Kept shots are
// then pulled to the destination and pushed through the fuji-import pipeline
// (EXIF restamp -> SHA-1 -> Immich upload -> checksum validation).
//
// Camera access defaults to aft-mtp-cli batch pulls: FUSE MTP mounts
// (go-mtpfs, aft-mtp-mount) do not work against the X-H2S, only the cli does.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/mtpcli"
	"github.com/zack/fuji-tools/internal/pipeline"
)

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:8787", "HTTP listen address (use 0.0.0.0:8787 to cull from another device)")
		backendName = flag.String("backend", "cli", "camera access: cli (aft-mtp-cli batch, works on X-H2S) or dir (local/pre-mounted directory)")
		root        = flag.String("root", "", "dir backend: directory containing the photos (a DCIM tree or NNN_FUJI folders)")
		cameraRoot  = flag.String("camera-root", "/SLOT 1/DCIM,/SLOT 2/DCIM", "cli backend: comma-separated camera DCIM paths; dir backend: DCIM path relative to --root (default autodetect)")
		sessionName = flag.String("session", "default", "session name (decisions persist per session)")
		cacheDir    = flag.String("cache-dir", "", "preview buffer directory (default: ~/.cache/fuji-cull/<session>)")
		ahead       = flag.Int("ahead", 150, "shots to buffer ahead of the cursor")
		behind      = flag.Int("behind", 50, "shots to keep buffered behind the cursor")
		evictMargin = flag.Int("evict-margin", 600, "distance from cursor at which buffered images are evicted (~12 MB each on disk)")
		batch       = flag.Int("batch", 6, "max files pulled per aft-mtp-cli invocation while prefetching")
		logPath     = flag.String("log", "", "log file path (default: ~/.local/share/fuji-cull/logs/<ts>.log)")

		dest        = flag.String("dest", "", "import destination directory (e.g. on the NAS); may also be set per-import in the UI")
		immichURL   = flag.String("immich-url", os.Getenv("IMMICH_URL"), "Immich server URL (or env IMMICH_URL)")
		immichKey   = flag.String("immich-key", os.Getenv("IMMICH_API_KEY"), "Immich API key (or env IMMICH_API_KEY)")
		immichAlbum = flag.String("immich-album", "", "Immich album for imported keepers (created if missing)")
		skipImmich  = flag.Bool("skip-immich", false, "import to disk only; skip Immich upload + validation")
		retries     = flag.Int("retries", 3, "retries for Immich upload validation gaps")
		uploadConc  = flag.Int("upload-concurrency", 4, "parallel Immich uploads")
		hashConc    = flag.Int("hash-concurrency", 4, "parallel SHA-1 workers")
	)
	flag.Parse()
	setupLogging(*logPath)

	if err := exif.EnsurePath(); err != nil {
		log.Fatalf("%v", err)
	}

	if !*skipImmich && (*immichURL == "" || *immichKey == "") {
		log.Printf("WARN: Immich URL/key not configured; imports will only copy to the destination (--skip-immich implied)")
		*skipImmich = true
	}

	var backend Backend
	switch *backendName {
	case "cli":
		if err := mtpcli.Ensure(); err != nil {
			log.Fatalf("%v", err)
		}
		backend = &cliBackend{roots: strings.Split(*cameraRoot, ",")}
	case "dir":
		if *root == "" {
			log.Fatalf("--root is required with --backend dir")
		}
		dcimRoots := []string{}
		if *cameraRoot != "" && !strings.Contains(*cameraRoot, "SLOT") {
			dcimRoots = strings.Split(*cameraRoot, ",")
		} else {
			var err error
			dcimRoots, err = findDCIMRoots(*root)
			if err != nil {
				log.Fatalf("%v", err)
			}
		}
		log.Printf("DCIM roots under %s: %v", *root, dcimRoots)
		backend = &dirBackend{root: *root, dcimRoots: dcimRoots}
	default:
		log.Fatalf("unknown --backend %q (want cli or dir)", *backendName)
	}

	home, _ := os.UserHomeDir()
	sessPath := filepath.Join(home, ".local", "share", "fuji-cull", "sessions", *sessionName+".json")
	session, err := loadSession(sessPath)
	if err != nil {
		log.Fatalf("session: %v", err)
	}
	cache := *cacheDir
	if cache == "" {
		cache = filepath.Join(home, ".cache", "fuji-cull", *sessionName)
	}

	app := &App{
		backend:     backend,
		session:     session,
		importer:    &Importer{},
		sessionName: *sessionName,
		dest:        *dest,
		album:       *immichAlbum,
		pipelineOpts: pipeline.Options{
			ImmichURL:         strings.TrimRight(*immichURL, "/"),
			ImmichKey:         *immichKey,
			SkipImmich:        *skipImmich,
			Retries:           *retries,
			UploadConcurrency: *uploadConc,
			HashConcurrency:   *hashConc,
		},
	}

	// Discovery runs in the background so the UI is reachable immediately;
	// /api/state reports progress until finishInit flips the app ready.
	go func() {
		log.Printf("discovering camera files (backend=%s)...", backend.Name())
		listings, err := backend.Discover(context.Background(), app.setDiscovery)
		if err != nil {
			log.Printf("discover: %v", err)
			app.setDiscoveryError(err)
			return
		}
		catalog := buildCatalog(listings)
		if len(catalog.Shots) == 0 {
			log.Printf("discover: no shots found")
			app.setDiscoveryError(fmt.Errorf("no shots found on camera"))
			return
		}
		log.Printf("catalog: %d shots", len(catalog.Shots))
		cursor := session.Cursor()
		if cursor < 0 || cursor >= len(catalog.Shots) {
			cursor = 0
			_ = session.SetCursor(0)
		}
		prefetch, err := newPrefetcher(catalog, backend, cache, *ahead, *behind, *evictMargin, *batch, cursor)
		if err != nil {
			log.Printf("prefetch: %v", err)
			app.setDiscoveryError(err)
			return
		}
		go prefetch.Run()
		app.finishInit(catalog, prefetch)
		log.Printf("fuji-cull ready: http://%s  (session=%s, backend=%s, %d shots, buffer %d ahead / %d behind)",
			*listen, *sessionName, backend.Name(), len(catalog.Shots), *ahead, *behind)
	}()

	srv := &http.Server{Addr: *listen, Handler: app.handler()}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down...")
		if app.isReady() {
			app.prefetch.Close()
		}
		_ = srv.Close()
		os.Exit(0)
	}()

	log.Printf("http server up at http://%s (camera discovery running in background)", *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}

func setupLogging(logPath string) {
	if logPath == "" {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".local", "share", "fuji-cull", "logs")
		_ = os.MkdirAll(dir, 0o755)
		logPath = filepath.Join(dir, fmt.Sprintf("fuji-cull-%s.log", time.Now().Format("20060102-150405")))
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: could not open log %q: %v\n", logPath, err)
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
