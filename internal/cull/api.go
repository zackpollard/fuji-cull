package cull

import (
	"context"

	"github.com/zack/fuji-tools/internal/photo"
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

// Shots returns the catalog in display order. Only valid once Ready.
func (a *App) Shots() []*photo.Shot { return a.catalog.Shots }

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
func (a *App) StartImport(dest, album string) error {
	d, al := a.dest, a.album
	if dest != "" {
		d = dest
	}
	if album != "" {
		al = album
	}
	return a.importer.Start(a, d, al)
}

// ImportState returns the current import status snapshot.
func (a *App) ImportState() ImportStatus { return a.importer.Status() }
