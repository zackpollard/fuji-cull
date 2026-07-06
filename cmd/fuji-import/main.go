// fuji-import: pull Fuji X camera files via MTP, mirror to disk, push to Immich, validate.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/mtpcli"
	"github.com/zack/fuji-tools/internal/photo"
	"github.com/zack/fuji-tools/internal/pipeline"
)

type Config struct {
	pipeline.Options
	CameraRoot string
	SkipPull   bool
	LogPath    string
}

func main() {
	cfg := parseFlags()
	setupLogging(cfg)

	log.Printf("=== fuji-import starting at %s ===", time.Now().Format(time.RFC3339))
	log.Printf("dest=%s camera-root=%q", cfg.Dest, cfg.CameraRoot)
	log.Printf("skip-pull=%v skip-immich=%v retries=%d upload-concurrency=%d",
		cfg.SkipPull, cfg.SkipImmich, cfg.Retries, cfg.UploadConcurrency)

	checkTools(cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(cfg.Dest, 0o755); err != nil {
		log.Fatalf("mkdir dest: %v", err)
	}

	// Phase 1: Discover (camera or local, depending on --skip-pull)
	var files []photo.FileEntry
	var err error
	if cfg.SkipPull {
		log.Printf("--- Phase 1: discovering local files (skip-pull) ---")
		files, err = mtpcli.DiscoverLocal(cfg.Dest)
	} else {
		log.Printf("--- Phase 1: discovering camera files ---")
		files, err = mtpcli.DiscoverCamera(ctx, cfg.CameraRoot, cfg.Dest)
	}
	if err != nil {
		log.Fatalf("discovery: %v", err)
	}
	log.Printf("Discovered %d files", len(files))
	if len(files) == 0 {
		log.Fatalf("no files found")
	}

	// Phase 2: Pull (if not skipped)
	if !cfg.SkipPull {
		log.Printf("--- Phase 2: pulling from camera ---")
		if err := mtpcli.Pull(ctx, cfg.CameraRoot, cfg.Dest, files, cfg.DryRun); err != nil {
			log.Fatalf("pull phase: %v", err)
		}
	}

	// Phases 3-6: restamp, hash, Immich upload + validate + retry
	if err := pipeline.Run(ctx, cfg.Options, files); err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("=== done ===")
}

func parseFlags() Config {
	cfg := Config{}
	flag.StringVar(&cfg.Dest, "dest", "", "destination directory (required)")
	flag.StringVar(&cfg.CameraRoot, "camera-root", "/SLOT 1/DCIM",
		"MTP path on camera to traverse")
	flag.StringVar(&cfg.ImmichURL, "immich-url", os.Getenv("IMMICH_URL"),
		"Immich server URL (or env IMMICH_URL)")
	flag.StringVar(&cfg.ImmichKey, "immich-key", os.Getenv("IMMICH_API_KEY"),
		"Immich API key (or env IMMICH_API_KEY)")
	flag.StringVar(&cfg.ImmichAlbum, "immich-album", "", "Immich album to add assets to (created if missing)")
	flag.BoolVar(&cfg.SkipImmich, "skip-immich", false, "skip Immich upload + validation")
	flag.BoolVar(&cfg.SkipPull, "skip-pull", false, "skip camera pull (validation-only mode)")
	flag.IntVar(&cfg.Retries, "retries", 3, "retries for Immich upload validation gaps")
	flag.IntVar(&cfg.UploadConcurrency, "upload-concurrency", 4, "parallel Immich uploads")
	flag.IntVar(&cfg.HashConcurrency, "hash-concurrency", 4, "parallel SHA-1 workers")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "show what would be done; no pull/upload")
	flag.StringVar(&cfg.LogPath, "log", "", "log file path (default: ~/.local/share/fuji-import/logs/<ts>.log)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "fuji-import: pull Fuji X camera files via MTP, mirror to disk, push to Immich, validate.\n\n")
		fmt.Fprintf(os.Stderr, "Usage: %s --dest PATH [options]\n\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s --dest /mnt/skynas/.../2026-05-21 \\\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "      --immich-url https://immich.example.com --immich-key XXX \\\n")
		fmt.Fprintf(os.Stderr, "      --immich-album 'Trip 2026'\n\n")
		fmt.Fprintf(os.Stderr, "  %s --dest /path --skip-pull --immich-url ... --immich-key ...\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "      # validation-only re-run (no camera needed)\n")
	}
	flag.Parse()

	if cfg.Dest == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --dest is required")
		flag.Usage()
		os.Exit(2)
	}
	if !cfg.SkipImmich {
		if cfg.ImmichURL == "" {
			fmt.Fprintln(os.Stderr, "ERROR: --immich-url required (or set IMMICH_URL)")
			os.Exit(2)
		}
		if cfg.ImmichKey == "" {
			fmt.Fprintln(os.Stderr, "ERROR: --immich-key required (or set IMMICH_API_KEY)")
			os.Exit(2)
		}
		cfg.ImmichURL = strings.TrimRight(cfg.ImmichURL, "/")
	}
	if cfg.UploadConcurrency < 1 {
		cfg.UploadConcurrency = 1
	}
	if cfg.HashConcurrency < 1 {
		cfg.HashConcurrency = 1
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	return cfg
}

func setupLogging(cfg Config) {
	logPath := cfg.LogPath
	if logPath == "" {
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".local", "share", "fuji-import", "logs")
		_ = os.MkdirAll(dir, 0o755)
		logPath = filepath.Join(dir, fmt.Sprintf("fuji-import-%s.log", time.Now().Format("20060102-150405")))
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: could not open log %q: %v\n", logPath, err)
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("logging to %s", logPath)
}

func checkTools(cfg Config) {
	missing := []string{}
	if !cfg.SkipPull {
		if err := mtpcli.Ensure(); err != nil {
			missing = append(missing, err.Error())
		}
	}
	if err := exif.EnsurePath(); err != nil {
		missing = append(missing, err.Error())
	}
	if len(missing) > 0 {
		log.Fatalf("missing required tools: %s", strings.Join(missing, ", "))
	}
}
