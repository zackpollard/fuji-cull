// Package gphoto drives gphoto2 for thumbnail retrieval. The X-H2S can enter
// a USB state where android-file-transfer's GetThumb takes ~10 s per request
// while libgphoto2's stays at ~0.1 s, so thumbnails go through gphoto2; bulk
// file transfer stays on aft-mtp-cli (fast on both stacks, and gphoto2 pays a
// per-invocation folder enumeration that aft does not).
package gphoto

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Ensure checks that gphoto2 is installed.
func Ensure() error {
	if _, err := exec.LookPath("gphoto2"); err != nil {
		return fmt.Errorf("gphoto2 not in PATH (pacman -S gphoto2)")
	}
	return nil
}

var (
	storeMu     sync.Mutex
	storePrefix string
)

var storeRe = regexp.MustCompile(`basedir=(/store_[0-9a-fA-F]+)`)

// StorePrefix discovers the camera's storage prefix (e.g. "/store_10000001")
// via --storage-info (fast; `gphoto2 -l` walks the whole tree and can take
// minutes on a 19k-file card). Success is cached for the process lifetime;
// failure is NOT — a camera that was asleep or mid-restart gets retried.
func StorePrefix(ctx context.Context) (string, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if storePrefix != "" {
		return storePrefix, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := run(cctx, "", "--storage-info")
	if m := storeRe.FindSubmatch([]byte(out)); m != nil {
		storePrefix = string(m[1])
		return storePrefix, nil
	}
	return "", fmt.Errorf("no basedir in gphoto2 --storage-info output: %v; output: %.200s", err, out)
}

// FetchThumbSpan pulls thumbnails for a contiguous 1-based index span within
// a camera folder into workDir. Only plain "N-M" ranges are used — this
// gphoto2 rejects comma lists — and indexes that are videos are skipped by
// gphoto2 itself without aborting the range. Results are self-identifying
// (gphoto2 names them thumb_<stem>.jpg); the returned map is stem -> path.
// Partial results are returned even when the run errors or is cancelled.
func FetchThumbSpan(ctx context.Context, cameraDir string, start, end int, workDir string) (map[string]string, error) {
	prefix, err := StorePrefix(ctx)
	if err != nil {
		return nil, err
	}
	if end < start {
		return map[string]string{}, nil
	}
	rangeArg := fmt.Sprintf("%d-%d", start, end)
	if start == end {
		rangeArg = fmt.Sprintf("%d", start)
	}
	_, runErr := run(ctx, workDir,
		"--folder", prefix+"/"+cameraDir,
		"--get-thumbnail", rangeArg,
	)

	got := map[string]string{}
	entries, _ := os.ReadDir(workDir)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "thumb_") {
			continue
		}
		stem := strings.TrimSuffix(strings.TrimPrefix(name, "thumb_"), filepath.Ext(name))
		if fi, err := e.Info(); err == nil && fi.Size() > 0 {
			got[stem] = filepath.Join(workDir, name)
		}
	}
	return got, runErr
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "gphoto2", args...)
	if dir != "" {
		c.Dir = dir
	}
	// Interrupt politely on cancellation so libgphoto2 closes the session.
	c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
	c.WaitDelay = 3 * time.Second
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	if err != nil {
		return out.String(), fmt.Errorf("gphoto2 %s: %w; output tail: %.300s",
			strings.Join(args, " "), err, tail(out.String(), 300))
	}
	return out.String(), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
