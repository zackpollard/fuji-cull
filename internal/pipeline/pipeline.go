// Package pipeline implements the local half of the import flow: EXIF mtime
// restamping, hashing, Immich upload, and checksum validation with retries.
// It operates on files already present on local disk (pulled by any transport).
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zack/fuji-tools/internal/exif"
	"github.com/zack/fuji-tools/internal/hashutil"
	"github.com/zack/fuji-tools/internal/immich"
	"github.com/zack/fuji-tools/internal/photo"
)

type Options struct {
	Dest              string
	ImmichURL         string
	ImmichKey         string
	ImmichAlbum       string
	SkipImmich        bool
	Retries           int
	UploadConcurrency int
	HashConcurrency   int
	DryRun            bool
	// Progress, when non-nil, receives (phase, done, total) updates.
	Progress func(phase string, done, total int)
}

func (o Options) progress(phase string, done, total int) {
	if o.Progress != nil {
		o.Progress(phase, done, total)
	}
}

// Run executes restamp -> hash -> upload -> validate (+retries) -> report
// over files that already exist locally. Returns an error if files remain
// missing in Immich after all retries.
func Run(ctx context.Context, opts Options, files []photo.FileEntry) error {
	if err := Finalize(opts, files); err != nil {
		return fmt.Errorf("finalize local: %w", err)
	}

	log.Printf("--- computing SHA-1 (%d files, concurrency=%d) ---", len(files), opts.HashConcurrency)
	if err := Hash(ctx, opts, files); err != nil {
		return fmt.Errorf("hash phase: %w", err)
	}

	if !opts.SkipImmich {
		client := immich.NewClient(opts.ImmichURL, opts.ImmichKey)
		var albumID string
		if opts.ImmichAlbum != "" {
			id, err := client.EnsureAlbum(ctx, opts.ImmichAlbum)
			if err != nil {
				return fmt.Errorf("ensure album %q: %w", opts.ImmichAlbum, err)
			}
			albumID = id
			log.Printf("Album %q -> id=%s", opts.ImmichAlbum, albumID)
		}

		log.Printf("--- uploading to Immich (concurrency=%d) ---", opts.UploadConcurrency)
		if err := Upload(ctx, opts, client, albumID, files); err != nil {
			return fmt.Errorf("upload phase: %w", err)
		}

		log.Printf("--- validating against Immich ---")
		missing, err := Validate(ctx, client, files)
		if err != nil {
			return fmt.Errorf("validate phase: %w", err)
		}

		for attempt := 1; attempt <= opts.Retries && len(missing) > 0; attempt++ {
			log.Printf("--- retry %d/%d for %d missing file(s) ---", attempt, opts.Retries, len(missing))
			if err := Upload(ctx, opts, client, albumID, missing); err != nil {
				log.Printf("retry upload error: %v", err)
			}
			missing, err = Validate(ctx, client, files)
			if err != nil {
				return fmt.Errorf("revalidation: %w", err)
			}
		}

		if len(missing) > 0 {
			log.Printf("ERROR: %d file(s) still missing in Immich after %d retries:",
				len(missing), opts.Retries)
			for _, f := range missing {
				log.Printf("  MISSING: %s", f.LocalPath)
			}
			Report(opts.Dest, files)
			return fmt.Errorf("%d file(s) missing in Immich after %d retries", len(missing), opts.Retries)
		}
		log.Printf("All %d files verified in Immich", len(files))
	}

	Report(opts.Dest, files)
	return nil
}

// Finalize populates Size on each FileEntry and restamps mtime from EXIF.
func Finalize(opts Options, files []photo.FileEntry) error {
	// Restamp mtimes for any subdir present.
	dirs := map[string]struct{}{}
	for _, f := range files {
		dirs[filepath.Dir(f.LocalPath)] = struct{}{}
	}
	if !opts.DryRun {
		for d := range dirs {
			if err := exif.RestampMtime(d); err != nil {
				log.Printf("WARN: restamp %s: %v", d, err)
			}
		}
	}
	// Populate Size.
	for i := range files {
		st, err := os.Stat(files[i].LocalPath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", files[i].LocalPath, err)
		}
		files[i].Size = st.Size()
	}
	return nil
}

// Hash computes SHA-1 for all files with bounded concurrency.
func Hash(ctx context.Context, opts Options, files []photo.FileEntry) error {
	type job struct{ idx int }
	jobs := make(chan job, opts.HashConcurrency)
	errs := make(chan error, len(files))
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	for i := 0; i < opts.HashConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				sum, b64, err := hashutil.SHA1File(files[j.idx].LocalPath)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", files[j.idx].LocalPath, err)
					continue
				}
				files[j.idx].SHA1 = sum
				files[j.idx].SHA1B64 = b64
				mu.Lock()
				done++
				opts.progress("hash", done, len(files))
				mu.Unlock()
			}
		}()
	}
	for i := range files {
		jobs <- job{idx: i}
	}
	close(jobs)
	wg.Wait()
	close(errs)

	var collected []string
	for e := range errs {
		collected = append(collected, e.Error())
	}
	if len(collected) > 0 {
		return errors.New("hash errors: " + strings.Join(collected, "; "))
	}
	return nil
}

// Upload pushes files to Immich with bounded concurrency and optionally adds
// them to an album.
func Upload(ctx context.Context, opts Options, client *immich.Client, albumID string, files []photo.FileEntry) error {
	if opts.DryRun {
		log.Printf("[dry-run] would upload %d files", len(files))
		return nil
	}
	type job struct{ idx int }
	jobs := make(chan job, opts.UploadConcurrency)
	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		ok, dup, fail  int
		toAlbum        []string
		failedMessages []string
	)

	for w := 0; w < opts.UploadConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				f := &files[j.idx]
				assetID, duplicate, err := client.Upload(ctx, f)
				mu.Lock()
				switch {
				case err != nil:
					fail++
					failedMessages = append(failedMessages, fmt.Sprintf("%s: %v", f.LocalPath, err))
				case duplicate:
					dup++
					f.AssetID = assetID
					if albumID != "" && assetID != "" {
						toAlbum = append(toAlbum, assetID)
					}
				default:
					ok++
					f.AssetID = assetID
					if albumID != "" {
						toAlbum = append(toAlbum, assetID)
					}
				}
				opts.progress("upload", ok+dup+fail, len(files))
				mu.Unlock()
			}
		}()
	}
	for i := range files {
		jobs <- job{idx: i}
	}
	close(jobs)
	wg.Wait()

	log.Printf("Upload summary: uploaded=%d duplicate=%d failed=%d", ok, dup, fail)
	for _, m := range failedMessages {
		log.Printf("  FAIL: %s", m)
	}

	if albumID != "" && len(toAlbum) > 0 {
		// dedupe assetIDs
		seen := map[string]struct{}{}
		uniq := make([]string, 0, len(toAlbum))
		for _, id := range toAlbum {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			uniq = append(uniq, id)
		}
		log.Printf("Adding %d assets to album", len(uniq))
		if err := client.AddToAlbum(ctx, albumID, uniq); err != nil {
			log.Printf("WARN: add to album failed: %v", err)
		}
	}

	return nil
}

// Validate bulk-checks all files against Immich by checksum and returns the
// ones Immich does not have yet.
func Validate(ctx context.Context, client *immich.Client, files []photo.FileEntry) ([]photo.FileEntry, error) {
	// Build checksums list (all hashed at this point).
	type item struct {
		idx  int
		csum string
	}
	items := make([]item, 0, len(files))
	for i, f := range files {
		if f.SHA1B64 == "" {
			return nil, fmt.Errorf("file %s has no SHA1 (hash phase incomplete)", f.LocalPath)
		}
		items = append(items, item{idx: i, csum: f.SHA1B64})
	}

	// POST in batches.
	const batch = 500
	missingIdx := map[int]struct{}{}
	for start := 0; start < len(items); start += batch {
		end := start + batch
		if end > len(items) {
			end = len(items)
		}
		sub := items[start:end]
		ids := make([]string, len(sub))
		csums := make([]string, len(sub))
		for i, it := range sub {
			ids[i] = fmt.Sprintf("%d", it.idx)
			csums[i] = it.csum
		}
		res, err := client.BulkCheck(ctx, ids, csums)
		if err != nil {
			return nil, fmt.Errorf("bulk-check batch %d-%d: %w", start, end, err)
		}
		for i, it := range sub {
			id := ids[i]
			r, ok := res[id]
			if !ok || r.Action == "accept" {
				// Immich would accept it (i.e., doesn't have it yet) => missing
				missingIdx[it.idx] = struct{}{}
			} else if r.AssetID != "" {
				files[it.idx].AssetID = r.AssetID
			}
		}
	}

	var missing []photo.FileEntry
	if len(missingIdx) > 0 {
		keys := make([]int, 0, len(missingIdx))
		for k := range missingIdx {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		missing = make([]photo.FileEntry, 0, len(keys))
		for _, k := range keys {
			missing = append(missing, files[k])
		}
	}
	log.Printf("Validation: %d/%d present in Immich, %d missing",
		len(files)-len(missing), len(files), len(missing))
	return missing, nil
}

// Report logs a per-folder summary of the processed files.
func Report(dest string, files []photo.FileEntry) {
	var totalBytes int64
	byFolder := map[string]int{}
	for _, f := range files {
		totalBytes += f.Size
		byFolder[f.Folder]++
	}
	log.Printf("=== report ===")
	log.Printf("Total files:    %d", len(files))
	log.Printf("Total bytes:    %d (%.2f GB)", totalBytes, float64(totalBytes)/1e9)
	folders := make([]string, 0, len(byFolder))
	for k := range byFolder {
		folders = append(folders, k)
	}
	sort.Strings(folders)
	for _, k := range folders {
		log.Printf("  %s: %d files", k, byFolder[k])
	}
	log.Printf("Destination:    %s", dest)
}
