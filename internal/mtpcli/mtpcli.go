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
	"strings"

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
// Each entry in cmds is a separate line of input.
func RunBatch(ctx context.Context, cmds ...string) (string, error) {
	if err := Ensure(); err != nil {
		return "", err
	}
	stdin := strings.NewReader(strings.Join(cmds, "\n") + "\n")
	c := exec.CommandContext(ctx, "aft-mtp-cli", "-b")
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
