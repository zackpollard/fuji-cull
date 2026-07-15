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
	bin := AftBin()
	if p := os.Getenv("FUJI_AFT"); p != "" {
		if st, err := os.Stat(p); err == nil && st.Mode()&0o111 != 0 {
			return nil
		}
		return fmt.Errorf("FUJI_AFT points at %q but it is not executable", p)
	}
	if _, err := exec.LookPath(bin); err != nil {
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
			if err != nil && TransportBroken(out) {
				RequestReset()
				NoteTransportResult(true)
			} else if err == nil {
				NoteTransportResult(false)
			}
			return out, err
		}
	}
	return out, err
}

func runBatchOnce(ctx context.Context, cmds ...string) (string, error) {
	stdin := strings.NewReader(strings.Join(cmds, "\n") + "\n")
	args := []string{"-b"}
	var extra []*os.File
	if f := USBFile(); f != nil {
		args = append(args, "--device-fd", "3") // ExtraFiles[0] lands at fd 3
		extra = []*os.File{f}
		if ConsumeReset() {
			args = append(args, "-R")
			log.Printf("usb: link degraded — attempting device reset on this invocation")
		}
	}
	c := exec.CommandContext(ctx, AftBin(), args...)
	c.ExtraFiles = extra
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

// LsProps lists a camera directory via bulk MTP GetObjectPropList (patched
// aft "lsprops") — two round-trips per folder instead of one per FILE
// (~90s → ~1s on a 19k-file card). Errors when the camera or binary lacks
// support; callers fall back to LsExt.
func LsProps(ctx context.Context, dir string) ([]Entry, error) {
	out, err := RunBatch(ctx, fmt.Sprintf(`lsprops %q`, dir))
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{ObjectID: f[0], Size: size, Name: f[2]})
	}
	return entries, nil
}

// DirEntryID is a directory child with its object handle.
type DirEntryID struct {
	ObjectID string
	Name     string
}

// LsIDs lists a directory's children with handles (parses ls "id\tname").
func LsIDs(ctx context.Context, dir string) ([]DirEntryID, error) {
	out, err := RunBatch(ctx, fmt.Sprintf(`cd %q`, dir), "ls")
	if err != nil {
		return nil, err
	}
	var entries []DirEntryID
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if _, err := strconv.ParseUint(f[0], 10, 64); err != nil {
			continue
		}
		entries = append(entries, DirEntryID{ObjectID: f[0], Name: f[1]})
	}
	return entries, nil
}

// AllEntry is one object from a card-wide bulk listing.
type AllEntry struct {
	ObjectID string
	Size     int64
	ParentID string
	Name     string
}

// LsPropsAll lists EVERY object on the device in three bulk requests — the
// only GetObjectPropList shape the X-H2S honors (handle 0xFFFFFFFF, depth
// 0). 19,702 objects arrived in one 689 KB response in field testing.
func LsPropsAll(ctx context.Context) ([]AllEntry, error) {
	out, err := RunBatch(ctx, "lsprops-all")
	if err != nil {
		return nil, err
	}
	var entries []AllEntry
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		if _, err := strconv.ParseUint(f[0], 10, 64); err != nil {
			continue
		}
		entries = append(entries, AllEntry{ObjectID: f[0], Size: size, ParentID: f[2], Name: f[3]})
	}
	return entries, nil
}

// LsHandles returns a folder's object handles — one always-supported MTP
// request, for diffing against a cached catalog.
func LsHandles(ctx context.Context, dir string) ([]string, error) {
	out, err := RunBatch(ctx, fmt.Sprintf(`ls-handles %q`, dir))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 1 {
			continue
		}
		if _, err := strconv.ParseUint(f[0], 10, 64); err != nil {
			continue
		}
		ids = append(ids, f[0])
	}
	return ids, nil
}

// InfoByIDs fetches name+size for specific handles (new files found by a
// handle diff) in one invocation.
func InfoByIDs(ctx context.Context, ids []string) ([]Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cmds := make([]string, 0, len(ids))
	for _, id := range ids {
		cmds = append(cmds, "info-id "+id)
	}
	out, err := RunBatch(ctx, cmds...)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{ObjectID: f[0], Size: size, Name: f[2]})
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
