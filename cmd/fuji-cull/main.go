// fuji-cull: cull Fuji camera photos straight off the camera — web UI at
// --listen, sharing its engine with the native GUI frontend.
//
// Photos stay on the camera while you review; a sliding window of full-size
// images is buffered to local disk so browsing is instant. Kept shots are
// then pulled to the destination and pushed through the fuji-import pipeline
// (EXIF restamp -> SHA-1 -> Immich upload -> checksum validation).
//
// Camera access defaults to aft-mtp-cli batch pulls: FUSE MTP mounts
// (go-mtpfs, aft-mtp-mount) do not work against the X-H2S, only the cli does.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zack/fuji-tools/internal/cull"
)

func main() {
	var o cull.Options
	flag.StringVar(&o.Listen, "listen", "127.0.0.1:8787", "HTTP listen address (use 0.0.0.0:8787 to cull from another device)")
	flag.StringVar(&o.BackendName, "backend", "cli", "camera access: cli (aft-mtp-cli batch, works on X-H2S) or dir (local/pre-mounted directory)")
	flag.StringVar(&o.Root, "root", "", "dir backend: directory containing the photos (a DCIM tree or NNN_FUJI folders)")
	flag.StringVar(&o.CameraRoot, "camera-root", "/SLOT 1/DCIM,/SLOT 2/DCIM", "cli backend: comma-separated camera DCIM paths; dir backend: DCIM path relative to --root (default autodetect)")
	flag.StringVar(&o.SessionName, "session", "default", "session name (decisions persist per session)")
	flag.StringVar(&o.CacheDir, "cache-dir", "", "image buffer directory (default: ~/.cache/fuji-cull/<session>)")
	flag.IntVar(&o.Ahead, "ahead", 150, "shots to buffer ahead of the cursor")
	flag.IntVar(&o.Behind, "behind", 50, "shots to keep buffered behind the cursor")
	flag.IntVar(&o.EvictMargin, "evict-margin", 600, "distance from cursor at which buffered images are evicted (~12 MB each on disk)")
	flag.IntVar(&o.Batch, "batch", 6, "max files pulled per aft-mtp-cli invocation while prefetching")
	logPath := flag.String("log", "", "log file path (default: ~/.local/share/fuji-cull/logs/<ts>.log)")

	flag.StringVar(&o.Dest, "dest", "", "import destination directory (e.g. on the NAS); may also be set per-import in the UI")
	flag.StringVar(&o.ImmichURL, "immich-url", os.Getenv("IMMICH_URL"), "Immich server URL (or env IMMICH_URL)")
	flag.StringVar(&o.ImmichKey, "immich-key", os.Getenv("IMMICH_API_KEY"), "Immich API key (or env IMMICH_API_KEY)")
	flag.StringVar(&o.ImmichAlbum, "immich-album", "", "Immich album for imported keepers (created if missing)")
	flag.BoolVar(&o.SkipImmich, "skip-immich", false, "import to disk only; skip Immich upload + validation")
	flag.IntVar(&o.Retries, "retries", 3, "retries for Immich upload validation gaps")
	flag.IntVar(&o.UploadConc, "upload-concurrency", 4, "parallel Immich uploads")
	flag.IntVar(&o.HashConc, "hash-concurrency", 4, "parallel SHA-1 workers")
	flag.Parse()
	setupLogging(*logPath)

	app, handler, err := cull.Start(o)
	if err != nil {
		log.Fatalf("%v", err)
	}

	srv := &http.Server{Addr: o.Listen, Handler: handler}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down...")
		app.Close()
		_ = srv.Close()
		os.Exit(0)
	}()

	log.Printf("http server up at http://%s (camera discovery running in background)", o.Listen)
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
