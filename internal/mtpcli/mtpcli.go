// Package mtpcli drives aft-mtp-cli (android-file-transfer) in batch mode for
// bulk discovery and pulls. MTP sessions are single-threaded, so all camera
// traffic here is sequential.
package mtpcli

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zack/fuji-tools/internal/photo"
)

// Ensure checks that aft-mtp-cli is installed.
func Ensure() error {
	if _, err := exec.LookPath("aft-mtp-cli"); err != nil {
		return fmt.Errorf("aft-mtp-cli not in PATH (pacman -S android-file-transfer)")
	}
	return nil
}

// RunBatch runs aft-mtp-cli in batch mode and returns combined output.
// Each entry in cmds is a separate line of input. A freshly-killed previous
// invocation can leave the device claimed for a moment, so "device already
// used" errors are retried briefly.
func RunBatch(ctx context.Context, cmds ...string) (string, error) {
	if err := Ensure(); err != nil {
		return "", err
	}
	var out string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		out, err = runBatchOnce(ctx, cmds...)
		if err == nil || !strings.Contains(out, "already used") {
			return out, err
		}
	}
	return out, err
}

func runBatchOnce(ctx context.Context, cmds ...string) (string, error) {
	stdin := strings.NewReader(strings.Join(cmds, "\n") + "\n")
	c := exec.CommandContext(ctx, "aft-mtp-cli", "-b")
	// On cancellation, interrupt instead of SIGKILL: a hard kill mid-USB
	// transaction wedges the camera's MTP session and the next invocation's
	// first command hangs on the stale state.
	c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
	c.WaitDelay = 3 * time.Second
	c.Stdin = stdin
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		return stdout.String() + stderr.String(),
			fmt.Errorf("aft-mtp-cli failed: %w; stderr=%s", err, stderr.String())
	}
	return stdout.String() + stderr.String(), nil
}

// Ls returns the raw `ls` output for a camera directory.
func Ls(ctx context.Context, dir string) (string, error) {
	return RunBatch(ctx, fmt.Sprintf(`cd %q`, dir), "ls")
}

// Entry is one file from an extended directory listing.
type Entry struct {
	ObjectID string
	Size     int64
	Name     string
}

// LsExt lists a camera directory with object IDs and sizes. Output lines look
// like: `2648  268435457  ExifJpeg  14379874 2026-05-10 15:02:50  DSCF0013.JPG`.
func LsExt(ctx context.Context, dir string) ([]Entry, error) {
	out, err := RunBatch(ctx, fmt.Sprintf(`lsext %q`, dir))
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		size, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{ObjectID: f[0], Size: size, Name: f[6]})
	}
	return entries, nil
}

// Get is one object to download to a local path.
type Get struct {
	ObjectID string
	Dest     string
}

// GetByIDs pulls objects in a single aft-mtp-cli invocation, in the given
// order — callers put the most urgently awaited file first. get-id needs no
// directory enumeration, so this is fast even in huge folders.
func GetByIDs(ctx context.Context, gets []Get) error {
	if len(gets) == 0 {
		return nil
	}
	cmds := make([]string, 0, len(gets))
	for _, g := range gets {
		cmds = append(cmds, fmt.Sprintf(`get-id %s %q`, g.ObjectID, g.Dest))
	}
	out, err := RunBatch(ctx, cmds...)
	if err != nil {
		return fmt.Errorf("get-id batch: %w; output tail:\n%s", err, truncate(out, 1000))
	}
	return nil
}

// GetThumbs pulls EXIF thumbnails (name->localPath) out of one camera
// directory in a single invocation. There is no get-thumb-by-id, so names
// MUST come from a real listing: a nonexistent name stalls aft-mtp-cli
// indefinitely — callers should also bound ctx.
func GetThumbs(ctx context.Context, dir string, pairs map[string]string) error {
	if len(pairs) == 0 {
		return nil
	}
	names := make([]string, 0, len(pairs))
	for name := range pairs {
		names = append(names, name)
	}
	sort.Strings(names)
	cmds := []string{fmt.Sprintf(`cd %q`, dir)}
	for _, name := range names {
		cmds = append(cmds, fmt.Sprintf(`get-thumb %q %q`, name, pairs[name]))
	}
	out, err := RunBatch(ctx, cmds...)
	if err != nil {
		return fmt.Errorf("get-thumb batch in %s: %w; output tail:\n%s", dir, err, truncate(out, 600))
	}
	return nil
}

// DiscoverCamera lists NNN_FUJI folders under cameraRoot and their DSCF files.
// dest is used to precompute each entry's LocalPath.
func DiscoverCamera(ctx context.Context, cameraRoot, dest string) ([]photo.FileEntry, error) {
	// List subfolders under camera root
	out, err := RunBatch(ctx,
		fmt.Sprintf(`cd %q`, cameraRoot),
		"ls",
	)
	if err != nil {
		return nil, err
	}
	folderSet := map[string]struct{}{}
	for _, m := range photo.FolderRe.FindAllString(out, -1) {
		folderSet[m] = struct{}{}
	}
	if len(folderSet) == 0 {
		return nil, fmt.Errorf("no %q-style folders found under %s; output was:\n%s",
			"NNN_FUJI", cameraRoot, truncate(out, 800))
	}
	folders := make([]string, 0, len(folderSet))
	for k := range folderSet {
		folders = append(folders, k)
	}
	sort.Strings(folders)
	log.Printf("Camera folders: %v", folders)

	var entries []photo.FileEntry
	for _, folder := range folders {
		out, err := RunBatch(ctx,
			fmt.Sprintf(`cd %q`, cameraRoot+"/"+folder),
			"ls",
		)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", folder, err)
		}
		filenames := map[string]struct{}{}
		for _, m := range photo.FileRe.FindAllString(out, -1) {
			filenames[m] = struct{}{}
		}
		names := make([]string, 0, len(filenames))
		for k := range filenames {
			names = append(names, k)
		}
		sort.Strings(names)
		log.Printf("  %s: %d files", folder, len(names))
		for _, name := range names {
			entries = append(entries, photo.FileEntry{
				Folder:    folder,
				Name:      name,
				LocalPath: filepath.Join(dest, folder, name),
			})
		}
	}
	return entries, nil
}

// Pull fetches files per-folder via aft-mtp-cli batch mode, skipping any that
// already exist locally. MTP session is single-threaded so this stays sequential.
func Pull(ctx context.Context, cameraRoot, dest string, files []photo.FileEntry, dryRun bool) error {
	if dryRun {
		log.Printf("[dry-run] would pull %d files", len(files))
		return nil
	}

	// Group by folder, build batch scripts, skip existing.
	byFolder := map[string][]photo.FileEntry{}
	for _, f := range files {
		byFolder[f.Folder] = append(byFolder[f.Folder], f)
	}

	folders := make([]string, 0, len(byFolder))
	for k := range byFolder {
		folders = append(folders, k)
	}
	sort.Strings(folders)

	for _, folder := range folders {
		subdest := filepath.Join(dest, folder)
		if err := os.MkdirAll(subdest, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", subdest, err)
		}

		var batch []string
		batch = append(batch, fmt.Sprintf(`cd %q`, cameraRoot+"/"+folder))
		got := 0
		for _, f := range byFolder[folder] {
			if _, err := os.Stat(f.LocalPath); err == nil {
				continue
			}
			batch = append(batch, fmt.Sprintf(`get %q %q`, f.Name, f.LocalPath))
			got++
		}
		log.Printf("Folder %s: %d to pull (others already present)", folder, got)
		if got == 0 {
			continue
		}
		out, err := RunBatch(ctx, batch...)
		if err != nil {
			return fmt.Errorf("pull folder %s: %w; output tail:\n%s",
				folder, err, truncate(out, 1500))
		}
		// best-effort error scan
		if strings.Contains(strings.ToLower(out), "error") {
			log.Printf("WARN: aft-mtp-cli output for %s contained 'error':\n%s",
				folder, truncate(out, 1500))
		}
	}
	return nil
}

// DiscoverLocal walks dest for previously pulled DSCF files.
func DiscoverLocal(dest string) ([]photo.FileEntry, error) {
	var out []photo.FileEntry
	err := filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !photo.FileRe.MatchString(name) {
			return nil
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return err
		}
		// expect <folder>/<name>
		parts := strings.Split(rel, string(filepath.Separator))
		folder := ""
		if len(parts) == 2 {
			folder = parts[0]
		}
		out = append(out, photo.FileEntry{
			Folder:    folder,
			Name:      name,
			LocalPath: p,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
