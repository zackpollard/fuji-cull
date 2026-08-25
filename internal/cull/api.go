package cull

import (
	"context"
	"os"
	"strings"

	"github.com/zack/fuji-tools/internal/photo"
	"github.com/zack/fuji-tools/internal/pipeline"
)

// GUI-facing accessors: the native frontend runs in-process with the same
// App the HTTP API serves, so both stay in sync by construction.

// Ready reports whether discovery finished and the prefetcher is running.
func (a *App) Ready() bool { return a.isReady() }

// Discovery returns the current discovery progress (pre-ready splash).
func (a *App) Discovery() (stage string, files int, errMsg string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.discStage, a.discFiles, a.discErr
}

// Nudge pokes the fetch pipeline: breakers and backoffs become eligible for
// an immediate probe. The mobile app calls it on foreground resume.
func (a *App) Nudge() {
	if a.isReady() {
		a.prefetch.Nudge()
	}
}

// Shots returns the catalog in display order. Only valid once Ready.
func (a *App) Shots() []*photo.Shot { return a.catalog.Shots }

// Rescan drops the catalog cache so the next engine start re-reads the whole
// card index (card swaps, in-camera deletions). Like POST /api/rescan it
// takes effect on restart — the running catalog is not rebuilt in place.
// Returns false when the backend has no cache to drop (e.g. dir backend).
func (a *App) Rescan() bool {
	if cb, ok := a.backend.(*cliBackend); ok && cb.cacheDir != "" {
		os.Remove(cb.cachePath())
		return true
	}
	return false
}

// ShotIndex returns the catalog position of a shot ID, or -1.
func (a *App) ShotIndex(id string) int {
	if i, ok := a.catalog.Index[id]; ok {
		return i
	}
	return -1
}

// Decisions returns a copy of the decision map.
func (a *App) Decisions() map[string]string { return a.session.Decisions() }

// SetDecision records keep/reject/"" for a shot.
func (a *App) SetDecision(id, decision string) error { return a.session.SetDecision(id, decision) }

// Cursor returns the persisted cursor index.
func (a *App) Cursor() int { return a.session.Cursor() }

// SetCursor persists the cursor and retargets the prefetch window.
func (a *App) SetCursor(i int) {
	_ = a.session.SetCursor(i)
	a.prefetch.SetCursor(i)
}

// WaitImage blocks until the shot's full image is buffered on disk and
// returns its cache path (triggering a priority camera fetch if needed).
func (a *App) WaitImage(ctx context.Context, id string) (string, error) {
	return a.prefetch.Wait(ctx, id)
}

// AltRendition reports the second rendition a shot carries ("RAF"), or "".
func (a *App) AltRendition(id string) string {
	s := a.catalog.Get(id)
	if s == nil {
		return ""
	}
	return AltExt(s)
}

// ImagePathIfReady returns the cached full-image path without waiting.
func (a *App) ImagePathIfReady(id string) (string, bool) {
	s := a.catalog.Get(id)
	if s == nil {
		return "", false
	}
	states := a.prefetch.Snapshot()
	if states[id] != "ready" {
		return "", false
	}
	return a.prefetch.displayPath(s), true
}

// ThumbPathIfReady returns the cached thumbnail path for a shot.
func (a *App) ThumbPathIfReady(id string) (string, bool) {
	s := a.catalog.Get(id)
	if s == nil || !a.prefetch.HasThumb(s) {
		return "", false
	}
	return a.prefetch.ThumbPath(s), true
}

// FetchStates returns shot ID -> "fetching"|"ready"|"failed".
func (a *App) FetchStates() map[string]string { return a.prefetch.Snapshot() }

// EnsureVideo queues a video shot for pulling to the local buffer. Any live
// camera stream is released first — the pull needs the link it holds.
func (a *App) EnsureVideo(id string) {
	a.prefetch.CloseStream()
	a.prefetch.Ensure(id)
}

// SetThumbHint retargets the background thumbnail sweep (grid viewport).
func (a *App) SetThumbHint(i int) { a.prefetch.SetThumbHint(i) }

// ThumbProgress returns per-shot thumb states and the cached count.
func (a *App) ThumbProgress() (string, int) { return a.prefetch.ThumbStates() }

// Orientations returns one byte per catalog shot: '1'-'8' known EXIF
// orientation, '0' unknown, '-' not applicable. Thumbnail files stay in
// sensor orientation on disk; renderers rotate at display time.
func (a *App) Orientations() string { return a.prefetch.OrientStates() }

// ImmichStates returns one byte per catalog shot: 1 already on Immich,
// 0 not uploaded, - unknown (or Immich not configured: empty string).
func (a *App) ImmichStates() string {
	if a.imcheck == nil {
		return ""
	}
	return a.imcheck.States()
}

// CameraSick reports tripped camera-transfer circuit breakers (the X-H2S
// stale-buffer bug); a power cycle is the only remedy.
func (a *App) CameraSick() (bulk, partial bool) { return a.prefetch.LinkSick() }

// pipeline returns the import pipeline options under the lock. Settings can be
// edited while the app runs, so this must not be read as a bare field.
func (a *App) pipeline() pipeline.Options {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.pipelineOpts
}

// ImmichSettings reports the current credentials for display. `active` is
// false when imports would be disk-only.
func (a *App) ImmichSettings() (url, key, album string, stack, active bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	o := a.pipelineOpts
	return o.ImmichURL, o.ImmichKey, a.album, o.ImmichStack, !o.SkipImmich
}

// SetImmich applies credentials entered in the app and persists them. Imports
// pick them up immediately — the pipeline reads its options per run.
//
// The "already on the server" badge sweep is built once at startup, so
// switching Immich ON here does not begin back-filling those badges until the
// next launch; imports themselves work right away.
func (a *App) SetImmich(url, key, album string, stack bool) {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	key = strings.TrimSpace(key)
	a.mu.Lock()
	a.pipelineOpts.ImmichURL = url
	a.pipelineOpts.ImmichKey = key
	a.pipelineOpts.ImmichStack = stack
	// Credentials are what decide whether an import can upload at all.
	a.pipelineOpts.SkipImmich = url == "" || key == ""
	a.album = album
	a.mu.Unlock()
	saveImmichDefaults(url, key, album, stack)
}

// FocusBest reports which shots are the sharpest frame of their burst, keyed by
// shot ID for direct lookup while drawing. Only bursts (2+ frames captured
// within a couple of seconds) are ever marked — see Prefetcher.BurstBest for
// why comparing focus scores across scenes would be meaningless.
func (a *App) FocusBest() map[string]bool {
	if !a.isReady() {
		return nil
	}
	ids := a.prefetch.BurstBest()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// CanStreamVideo reports whether the shot's video can play by streaming
// straight off the camera (no full pull). False during imports — the import
// owns the link for minutes and a stream session would fight it.
func (a *App) CanStreamVideo(id string) bool {
	s := a.catalog.Get(id)
	if s == nil || a.importer.Status().Running {
		return false
	}
	return a.prefetch.CanStream(s, s.DisplayExt())
}

// VideoPathIfReady returns the buffered local path of a video shot.
// VideoStreamPreview reports whether camera streaming of this video hits the
// 4 GiB partial-read ceiling. When it does, only a preview streams (the full
// clip needs a local pull, L), and fraction is the share of the clip that
// preview covers (streamable bytes / total) — used to locate the preview wall
// on the playback timeline.
func (a *App) VideoStreamPreview(id string) (limited bool, fraction float64) {
	s := a.catalog.Get(id)
	if s == nil || s.Kind != "video" {
		return false, 0
	}
	ext := s.DisplayExt()
	total := s.Sizes[ext]
	if total <= streamPartialLimit {
		return false, 0
	}
	return true, float64(streamPartialLimit) / float64(total)
}

func (a *App) VideoPathIfReady(id string) (string, bool) {
	s := a.catalog.Get(id)
	if s == nil || s.Kind != "video" {
		return "", false
	}
	if a.prefetch.Snapshot()[id] != "ready" {
		return "", false
	}
	return a.prefetch.displayPath(s), true
}

// Defaults returns the configured import destination and album.
func (a *App) Defaults() (dest, album string) { return a.dest, a.album }

// StartImport kicks off an import of keepers (same path the web UI uses).
func (a *App) StartImport(dest, album string, opt ImportOptions) error {
	d, al := a.dest, a.album
	if dest != "" {
		d = dest
	}
	if album != "" {
		al = album
	}
	return a.importer.Start(a, d, al, opt)
}

// PendingImport reports what an import would carry and what it would hold back.
func (a *App) PendingImportCounts(includeDone bool) (shots, skipped int) {
	if !a.isReady() {
		return 0, 0
	}
	return a.PendingImport(includeDone)
}

// ImportedCount is how many shots are recorded as already imported.
func (a *App) ImportedCount() int { return len(a.session.ImportedKeys()) }

// ClearImported forgets every imported marker; decisions are untouched.
func (a *App) ClearImported() (int, error) { return a.session.ClearImported() }

// ImportState returns the current import status snapshot.
func (a *App) ImportState() ImportStatus { return a.importer.Status() }
