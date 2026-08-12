package cull

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

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
	Date     string // capture day "2006-01-02"; "" unknown
	Taken    string // camera's raw timestamp, kept for identity checking
	ObjectID string // MTP object ID (cli backend)
}

// captureDay normalizes PTP datetimes ("20260714T101530") and lsext-style
// dates ("2026-07-14 ...") to a grouping day, or "".
func captureDay(raw string) string {
	if len(raw) >= 10 && raw[4] == '-' && raw[7] == '-' {
		return raw[:10]
	}
	if len(raw) >= 8 {
		allDigits := true
		for _, r := range raw[:8] {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return raw[:4] + "-" + raw[4:6] + "-" + raw[6:8]
		}
	}
	return ""
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
		if s.Date == "" && it.Date != "" {
			s.Date = it.Date
		}
		// The JPG's timestamp is the one to verify against: it is the file the
		// viewer pulls, and a RAF sidecar shares the same capture moment.
		if it.Taken != "" && (s.Taken == "" || ext == "JPG") {
			s.Taken = it.Taken
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
	// device-independent sync keys + the two reverse indexes (see canonical.go)
	cat.Canonical, cat.Legacy = assignCanonicalKeys(ordered)
	return cat
}

/* ── aft-mtp-cli backend ─────────────────────────────────── */

type cliBackend struct {
	roots    []string // camera-absolute DCIM paths, e.g. "/SLOT 1/DCIM"
	cacheDir string   // catalog cache home; empty disables caching
	probed   bool     // lsprops-probe fired once this run
	identity string   // "<Model> <Serial>" from device-info; the sync namespace
}

// CameraIdentity returns "<model> <serial>" for the sync namespace, matching the
// iOS ICC path so the same body keys the same session on every platform. Empty
// until Discover has run device-info (or if the camera reports no serial).
func (b *cliBackend) CameraIdentity() string { return b.identity }

// catalogCache persists folder listings between runs; cached folders
// refresh via a one-request handle diff on attach (new files fetched
// individually, in-camera deletions dropped) — minutes down to seconds on
// a 19k-file card. POST /api/rescan (or deleting the file) forces a full
// re-read.
type catalogCache struct {
	Version int                  `json:"version"`
	Folders map[string][]listing `json:"folders"` // key: root + "/" + folder
}

// cacheVersion bumps when listing gains fields (v2: capture dates) so old
// caches take one fast re-list instead of serving incomplete data.
const cacheVersion = 2

func (b *cliBackend) cachePath() string {
	return filepath.Join(b.cacheDir, "catalog-cache.json")
}

func (b *cliBackend) loadCache() *catalogCache {
	c := &catalogCache{Folders: map[string][]listing{}}
	if b.cacheDir == "" {
		return c
	}
	raw, err := os.ReadFile(b.cachePath())
	if err != nil {
		return c
	}
	if json.Unmarshal(raw, c) != nil || c.Folders == nil || c.Version != cacheVersion {
		c.Folders = map[string][]listing{}
		c.Version = cacheVersion
	}
	return c
}

func (b *cliBackend) saveCache(c *catalogCache) {
	if b.cacheDir == "" {
		return
	}
	c.Version = cacheVersion
	raw, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := b.cachePath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, b.cachePath())
	}
}

// listFolder prefers the bulk lsprops path (2 MTP round-trips per folder);
// an error disables it for the run (old binary / unsupported camera) and
// an empty result falls back for just that folder.
func (b *cliBackend) listFolder(ctx context.Context, dir string, bulkOK *bool) ([]mtpcli.Entry, error) {
	if *bulkOK {
		entries, err := mtpcli.LsProps(ctx, dir)
		if err == nil && len(entries) > 0 {
			return entries, nil
		}
		if err != nil {
			log.Printf("bulk listing unavailable (falling back to per-file lsext): %.150v", err)
			*bulkOK = false
		} else {
			log.Printf("bulk listing returned no entries for %s — using lsext for it", dir)
			if !b.probed {
				// one-shot diagnostic: which GetObjectPropList shapes does
				// this camera actually honor? (the field log answers it)
				b.probed = true
				if out, perr := mtpcli.RunBatch(ctx, fmt.Sprintf("lsprops-probe %q", dir)); perr == nil {
					log.Printf("lsprops probe results:\n%s", strings.TrimSpace(out))
				}
			}
		}
	}
	return mtpcli.LsExt(ctx, dir)
}

// deltaFolder refreshes a cached folder listing with a handle diff: one
// bulk property listing (two requests) rather than per-file info, so a
// re-attach costs seconds instead of the ~40s a full re-list took on a
// 7,983-file folder. Deletions in camera drop out of the catalog
// automatically, and capture dates are reused from cache by filename.
func (b *cliBackend) deltaFolder(ctx context.Context, dir, rel, folder string, cached []listing, group []mtpcli.AllEntry) ([]listing, bool, error) {
	// Re-read the folder's handle->name bindings every time rather than
	// trusting the cached ones.
	//
	// The old diff only asked which handles still EXIST, and kept the cached
	// name for any that did. But a camera is free to rebind a live handle to a
	// different object — after deletes, a format, or simply more shooting —
	// and this card did exactly that, shifting every binding by one file. The
	// cache then paired the right name and size with the wrong handle, so each
	// fetch pulled a DIFFERENT photo and stopped at its length. The size check
	// turned that into an unkillable retry loop; two files of equal size would
	// instead have been culled and uploaded as the wrong image.
	//
	// lsprops is two bulk requests per folder, so the authoritative answer is
	// cheap. The cache still earns its keep: it carries capture dates, which
	// cost a per-file lookup, and those are keyed by NAME — the identity that
	// does not move.
	// `ls` is the authoritative handle->name mapping and is supported
	// everywhere; lsprops would give sizes too but this camera answers it with
	// UnsupportedSpecByDepth — and does so as a successful exit with no rows,
	// which is why an earlier attempt read every folder as empty.
	prevByName := make(map[string]listing, len(cached))
	for _, l := range cached {
		prevByName[l.Name] = l
	}
	// The card-wide listing already has everything: handle, name, size and
	// date, authoritative and free. Use it when discovery managed to fetch it.
	if len(group) > 0 {
		fresh := make([]listing, 0, len(group))
		rebound := 0
		for _, e := range group {
			if _, _, ok := photo.SplitMedia(e.Name); !ok {
				continue
			}
			if old, ok := prevByName[e.Name]; ok && old.ObjectID != e.ObjectID {
				rebound++
			}
			fresh = append(fresh, listing{
				Dir: rel, Folder: folder, Name: e.Name,
				Size: e.Size, Date: captureDay(e.Date), Taken: e.Date, ObjectID: e.ObjectID,
			})
		}
		changed := rebound > 0 || len(fresh) != len(cached)
		if rebound > 0 {
			log.Printf("  %s: %d handle(s) now point at a different file — the camera rebound them; rebuilt from the card", rel, rebound)
		}
		return fresh, changed, nil
	}

	live, err := mtpcli.LsIDs(ctx, dir)
	if err != nil {
		return nil, false, err
	}
	if len(live) == 0 && len(cached) > 0 {
		// A folder we have seen files in does not become empty; treat it as a
		// listing failure so the caller re-lists rather than silently dropping
		// every shot in it.
		return nil, false, fmt.Errorf("listing returned no entries for a folder with %d cached files", len(cached))
	}
	prev := make(map[string]listing, len(cached))
	for _, l := range cached {
		prev[l.Name] = l
	}

	fresh := make([]listing, 0, len(live))
	var unknown []string // handles whose size/date we do not have cached
	rebound := 0
	for _, e := range live {
		if _, _, ok := photo.SplitMedia(e.Name); !ok {
			continue
		}
		old, known := prev[e.Name]
		if known && old.ObjectID != e.ObjectID {
			rebound++
		}
		l := listing{Dir: rel, Folder: folder, Name: e.Name, ObjectID: e.ObjectID}
		if known {
			// size and capture time belong to the file, not the handle
			l.Size, l.Date, l.Taken = old.Size, old.Date, old.Taken
		}
		if l.Size == 0 || l.Date == "" {
			unknown = append(unknown, e.ObjectID)
		}
		fresh = append(fresh, l)
	}

	// Only files we have never described cost a lookup.
	if len(unknown) > 0 {
		entries, err := mtpcli.InfoByIDs(ctx, unknown)
		if err != nil {
			return nil, false, err
		}
		byID := make(map[string]mtpcli.Entry, len(entries))
		for _, e := range entries {
			byID[e.ObjectID] = e
		}
		for i := range fresh {
			if fresh[i].Size != 0 && fresh[i].Date != "" {
				continue
			}
			if e, ok := byID[fresh[i].ObjectID]; ok {
				fresh[i].Size = e.Size
				fresh[i].Date = captureDay(e.Date)
				fresh[i].Taken = e.Date
			}
		}
	}

	added := len(fresh) - len(prev)
	changed := rebound > 0 || added != 0 || len(unknown) > 0
	if rebound > 0 {
		log.Printf("  %s: %d handle(s) now point at a different file — the camera rebound them; rebuilt from the card", rel, rebound)
	} else if changed {
		log.Printf("  %s: %d file(s), %d newly described (refreshed)", rel, len(fresh), len(unknown))
	}
	return fresh, changed, nil
}

func (b *cliBackend) Name() string { return "cli" }

func (b *cliBackend) Discover(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
	// Identify the body for the sync namespace (best-effort, short-bounded so a
	// camera that doesn't answer device-info never stalls discovery).
	if b.identity == "" {
		idCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if id := mtpcli.DeviceInfo(idCtx); id != "" {
			b.identity = id
			log.Printf("camera: identity %q", id)
		}
		cancel()
	}
	cache := b.loadCache()
	usedCache, cacheDirty := 0, false
	bulkOK := true
	// Card-wide bulk listing (3 MTP requests for EVERYTHING — the only
	// GetObjectPropList shape the X-H2S honors) fetched lazily on the
	// first uncached folder; entries grouped by parent handle.
	var byParent map[string][]mtpcli.AllEntry
	allTried := false
	// Try the card-wide listing up front: three requests for every object's
	// handle, name, size and date. That is cheaper than per-folder listing AND
	// it is what lets cached folders be re-validated for free — handles move,
	// and a cache that trusts them serves the wrong object.
	if all, err := mtpcli.LsPropsAll(ctx); err == nil && len(all) > 0 {
		byParent = make(map[string][]mtpcli.AllEntry, 64)
		for _, e := range all {
			byParent[e.ParentID] = append(byParent[e.ParentID], e)
		}
		allTried = true
		log.Printf("catalog: card-wide bulk listing — %d objects in 3 requests", len(all))
	} else if err != nil {
		log.Printf("card-wide bulk listing unavailable (%.120v) — per-folder listing", err)
	}
	var out []listing
	// Roots to try: the configured DCIM paths, then the storage root as a
	// fallback. A Sony body in MTP mode publishes no DCIM tree at all — its
	// media sits in date-named folders at the top level — so the Fuji
	// defaults find nothing on one and the fallback is what makes it work
	// without being told.
	roots := append([]string{}, b.roots...)
	if !slices.Contains(roots, "/") {
		roots = append(roots, "/")
	}
	// aft-mtp-cli's `cd` does not fail on a path the camera doesn't have: the
	// Sony answers the following `ls` from the storage root regardless. Taken
	// at face value that invents a full set of folders under every configured
	// root, each of which then costs a listing to discover it is empty. So
	// list the storage root once and treat any root that echoes it as absent.
	rootIDs := map[string]bool{}
	if entries, err := mtpcli.LsIDs(ctx, "/"); err == nil {
		for _, d := range entries {
			rootIDs[d.ObjectID] = true
		}
	}
	for _, root := range roots {
		if root == "/" && len(out) > 0 {
			continue // the configured roots already yielded media
		}
		progress(root, len(out))
		dirEntries, err := mtpcli.LsIDs(ctx, root)
		if err != nil {
			log.Printf("camera root %s: %v (skipping)", root, err)
			continue
		}
		if root != "/" && len(dirEntries) > 0 && len(rootIDs) > 0 && sameObjects(dirEntries, rootIDs) {
			log.Printf("camera root %s: not present (the camera answered from the storage root) — skipping", root)
			continue
		}
		folderIDs := map[string]string{}
		var folders []string
		for _, d := range dirEntries {
			if photo.IsMediaFolder(d.Name) {
				folders = append(folders, d.Name)
				folderIDs[d.Name] = d.ObjectID
			}
		}
		if len(folders) == 0 {
			log.Printf("camera root %s: no media folders (skipping)", root)
			continue
		}
		sort.Strings(folders)
		for _, folder := range folders {
			key := joinCamera(root, folder)
			rel := filepath.Join(trimSlash(root), folder)
			progress(rel, len(out))
			// cached folders refresh via a one-request handle diff; only
			// never-seen folders pay for a listing
			if cached, ok := cache.Folders[key]; ok {
				fresh, changed, err := b.deltaFolder(ctx, joinCamera(root, folder), rel, folder, cached, byParent[folderIDs[folder]])
				if err == nil {
					out = append(out, fresh...)
					usedCache++
					if changed {
						cache.Folders[key] = fresh
						cacheDirty = true
					}
					continue
				}
				log.Printf("  %s: handle diff failed (%v) — full re-list", rel, err)
			}
			if byParent == nil && !allTried {
				allTried = true
				if all, err := mtpcli.LsPropsAll(ctx); err == nil && len(all) > 0 {
					byParent = make(map[string][]mtpcli.AllEntry, 64)
					for _, e := range all {
						byParent[e.ParentID] = append(byParent[e.ParentID], e)
					}
					log.Printf("catalog: card-wide bulk listing — %d objects in 3 requests", len(all))
				} else if err != nil {
					log.Printf("card-wide bulk listing unavailable (%.120v) — per-folder listing", err)
				}
			}
			fresh := []listing{}
			if group, ok := byParent[folderIDs[folder]]; byParent != nil && ok {
				for _, e := range group {
					if _, _, ok := photo.SplitMedia(e.Name); !ok {
						continue
					}
					fresh = append(fresh, listing{
						Dir: rel, Folder: folder, Name: e.Name,
						Size: e.Size, Date: captureDay(e.Date), Taken: e.Date, ObjectID: e.ObjectID,
					})
				}
			} else {
				entries, err := b.listFolder(ctx, joinCamera(root, folder), &bulkOK)
				if err != nil {
					return nil, fmt.Errorf("list %s/%s: %w", root, folder, err)
				}
				for _, e := range entries {
					if _, _, ok := photo.SplitMedia(e.Name); !ok {
						continue
					}
					fresh = append(fresh, listing{
						Dir: rel, Folder: folder, Name: e.Name,
						Size: e.Size, Date: captureDay(e.Date), Taken: e.Date, ObjectID: e.ObjectID,
					})
				}
			}
			log.Printf("  %s: %d files", rel, len(fresh))
			out = append(out, fresh...)
			cache.Folders[key] = fresh
			cacheDirty = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no camera media found under roots %v", roots)
	}
	if usedCache > 0 {
		log.Printf("catalog: %d folders served from cache (settings → full rescan to re-read)", usedCache)
	}
	if cacheDirty {
		b.saveCache(cache)
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

// sameObjects reports whether a directory listing is exactly the storage
// root's — the signature of a `cd` the camera silently ignored.
func sameObjects(entries []mtpcli.DirEntryID, rootIDs map[string]bool) bool {
	if len(entries) != len(rootIDs) {
		return false
	}
	for _, d := range entries {
		if !rootIDs[d.ObjectID] {
			return false
		}
	}
	return true
}

// joinCamera joins a camera-absolute root to a folder without doubling the
// separator — the storage root is "/" itself.
func joinCamera(root, folder string) string {
	return strings.TrimSuffix(root, "/") + "/" + folder
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
			if !folder.IsDir() || !photo.IsMediaFolder(folder.Name()) {
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
				var day string
				if info, err := f.Info(); err == nil {
					size = info.Size()
					// no EXIF read here: mtime is the capture day for local
					// trees (and what the timeline groups by)
					day = info.ModTime().Format("2006-01-02")
				}
				out = append(out, listing{Dir: rel, Folder: folder.Name(), Name: f.Name(), Size: size, Date: day})
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
			if e.IsDir() && photo.IsMediaFolder(e.Name()) {
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
