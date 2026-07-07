package cull

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/zack/fuji-tools/internal/gphoto"
	"github.com/zack/fuji-tools/internal/mtpcli"
	"github.com/zack/fuji-tools/internal/photo"
)

// Backend abstracts how camera files are listed and pulled. The X-H2S rejects
// FUSE mounts (go-mtpfs and aft-mtp-mount both failed against it) but works
// reliably with aft-mtp-cli batch mode, so that is the default; the dir
// backend covers local directories for testing and pre-mounted filesystems.
type Backend interface {
	Name() string
	// Discover lists all Fuji media files (dir is relative to the camera
	// root), reporting progress as folders are enumerated.
	Discover(ctx context.Context, progress func(stage string, files int)) ([]listing, error)
	// Fetch pulls camera files to local destination paths.
	Fetch(ctx context.Context, items []fetchItem) error
	// LocalPath returns a directly readable path for a shot's file when the
	// backend exposes one (dir backend only); streaming videos needs this.
	LocalPath(s *photo.Shot, ext string) (string, bool)
}

type listing struct {
	Dir      string // camera dir relative to root, e.g. "SLOT 1/DCIM/151_FUJI"
	Folder   string // base folder name, e.g. "151_FUJI"
	Name     string // e.g. "DSCF0001.JPG"
	Size     int64
	ObjectID string // MTP object ID (cli backend)
}

type fetchItem struct {
	CameraDir string // listing.Dir
	Name      string
	ObjectID  string // set on the cli backend; enables enumeration-free get-id
	Dest      string // local path to write
}

// buildCatalog groups listings into RAF+JPG-paired shots, ordered by folder+frame.
func buildCatalog(items []listing) *Catalog {
	type key struct{ dir, base string }
	shots := map[key]*photo.Shot{}
	for _, it := range items {
		base, ext, ok := photo.SplitMedia(it.Name)
		if !ok {
			continue
		}
		k := key{dir: it.Dir, base: base}
		s := shots[k]
		if s == nil {
			s = &photo.Shot{
				ID:        it.Dir + "/" + base,
				CameraDir: it.Dir,
				Folder:    it.Folder,
				Base:      base,
				Files:     map[string]string{},
				Sizes:     map[string]int64{},
				ObjectIDs: map[string]string{},
			}
			shots[k] = s
		}
		s.Files[ext] = it.Name
		if it.Size > 0 {
			s.Sizes[ext] = it.Size
		}
		if it.ObjectID != "" {
			s.ObjectIDs[ext] = it.ObjectID
		}
	}

	ordered := make([]*photo.Shot, 0, len(shots))
	for _, s := range shots {
		s.Kind = "video"
		for ext := range s.Files {
			if photo.ShotKind(ext) == "photo" {
				s.Kind = "photo"
				break
			}
		}
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Folder != ordered[j].Folder {
			return ordered[i].Folder < ordered[j].Folder
		}
		return ordered[i].Base < ordered[j].Base
	})

	cat := &Catalog{Shots: ordered, Index: map[string]int{}}
	for i, s := range ordered {
		cat.Index[s.ID] = i
	}
	return cat
}

/* ── aft-mtp-cli backend ─────────────────────────────────── */

type cliBackend struct {
	roots []string // camera-absolute DCIM paths, e.g. "/SLOT 1/DCIM"
}

func (b *cliBackend) Name() string { return "cli" }

func (b *cliBackend) Discover(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
	var out []listing
	for _, root := range b.roots {
		progress(root, len(out))
		lsOut, err := mtpcli.Ls(ctx, root)
		if err != nil {
			log.Printf("camera root %s: %v (skipping)", root, err)
			continue
		}
		folderSet := map[string]struct{}{}
		for _, m := range photo.FolderRe.FindAllString(lsOut, -1) {
			folderSet[m] = struct{}{}
		}
		if len(folderSet) == 0 {
			log.Printf("camera root %s: no NNN_FUJI folders (skipping)", root)
			continue
		}
		folders := make([]string, 0, len(folderSet))
		for k := range folderSet {
			folders = append(folders, k)
		}
		sort.Strings(folders)
		for _, folder := range folders {
			progress(filepath.Join(trimSlash(root), folder), len(out))
			entries, err := mtpcli.LsExt(ctx, root+"/"+folder)
			if err != nil {
				return nil, fmt.Errorf("list %s/%s: %w", root, folder, err)
			}
			rel := filepath.Join(trimSlash(root), folder)
			count := 0
			for _, e := range entries {
				if _, _, ok := photo.SplitMedia(e.Name); !ok {
					continue
				}
				out = append(out, listing{
					Dir: rel, Folder: folder, Name: e.Name,
					Size: e.Size, ObjectID: e.ObjectID,
				})
				count++
			}
			log.Printf("  %s: %d files", rel, count)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Fuji media found under camera roots %v", b.roots)
	}
	return out, nil
}

func (b *cliBackend) Fetch(ctx context.Context, items []fetchItem) error {
	// get-id needs no directory context, so one invocation covers everything
	// regardless of folder, with no enumeration of huge DCIM folders. Item
	// order is preserved: the caller puts the most urgent file first.
	gets := make([]mtpcli.Get, 0, len(items))
	for _, it := range items {
		if it.ObjectID == "" {
			return fmt.Errorf("no MTP object ID for %s/%s", it.CameraDir, it.Name)
		}
		gets = append(gets, mtpcli.Get{ObjectID: it.ObjectID, Dest: it.Dest})
	}
	return mtpcli.GetByIDs(ctx, gets)
}

func (b *cliBackend) LocalPath(s *photo.Shot, ext string) (string, bool) { return "", false }

// ThumbFetcher is implemented by backends that can pull EXIF thumbnails
// without transferring the main image. Selection is a contiguous span of
// 1-based file indexes within the camera folder; results are self-identified
// by filename stem.
type ThumbFetcher interface {
	FetchThumbSpan(ctx context.Context, cameraDir string, start, end int, workDir string) (map[string]string, error)
}

// Thumbnails go through gphoto2 rather than aft-mtp-cli: the X-H2S can enter
// a USB state where aft's GetThumb takes ~10 s per request while libgphoto2
// stays at ~0.1 s. Bulk transfers stay on aft (no per-invocation folder
// enumeration there).
func (b *cliBackend) FetchThumbSpan(ctx context.Context, cameraDir string, start, end int, workDir string) (map[string]string, error) {
	return gphoto.FetchThumbSpan(ctx, cameraDir, start, end, workDir)
}

func trimSlash(p string) string {
	for len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}

/* ── local directory backend (testing / pre-mounted fs) ─────── */

type dirBackend struct {
	root      string
	dcimRoots []string // relative to root
}

func (b *dirBackend) Name() string { return "dir" }

func (b *dirBackend) Discover(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
	var out []listing
	for _, dcim := range b.dcimRoots {
		progress(dcim, len(out))
		dcimAbs := filepath.Join(b.root, dcim)
		folders, err := os.ReadDir(dcimAbs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dcimAbs, err)
		}
		for _, folder := range folders {
			if !folder.IsDir() || !photo.FolderRe.MatchString(folder.Name()) {
				continue
			}
			rel := filepath.Join(dcim, folder.Name())
			if dcim == "." {
				rel = folder.Name()
			}
			files, err := os.ReadDir(filepath.Join(b.root, rel))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", rel, err)
			}
			count := 0
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				if _, _, ok := photo.SplitMedia(f.Name()); !ok {
					continue
				}
				var size int64
				if info, err := f.Info(); err == nil {
					size = info.Size()
				}
				out = append(out, listing{Dir: rel, Folder: folder.Name(), Name: f.Name(), Size: size})
				count++
			}
			log.Printf("  %s: %d files", rel, count)
		}
	}
	return out, nil
}

func (b *dirBackend) Fetch(ctx context.Context, items []fetchItem) error {
	for _, it := range items {
		if err := copyFile(filepath.Join(b.root, it.CameraDir, it.Name), it.Dest); err != nil {
			return err
		}
	}
	return nil
}

func (b *dirBackend) LocalPath(s *photo.Shot, ext string) (string, bool) {
	name, ok := s.Files[ext]
	if !ok {
		return "", false
	}
	return filepath.Join(b.root, s.CameraDir, name), true
}

// findDCIMRoots returns paths relative to root that contain NNN_FUJI folders.
// Handles "<storage>/DCIM", a bare "DCIM", and NNN_FUJI folders directly in root.
func findDCIMRoots(root string) ([]string, error) {
	var candidates []string
	hasFujiDirs := func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.IsDir() && photo.FolderRe.MatchString(e.Name()) {
				return true
			}
		}
		return false
	}
	if hasFujiDirs(root) {
		candidates = append(candidates, ".")
	}
	if hasFujiDirs(filepath.Join(root, "DCIM")) {
		candidates = append(candidates, "DCIM")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "DCIM" {
			continue
		}
		sub := filepath.Join(e.Name(), "DCIM")
		if hasFujiDirs(filepath.Join(root, sub)) {
			candidates = append(candidates, sub)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no DCIM folders with NNN_FUJI subdirectories found under %s", root)
	}
	return candidates, nil
}
