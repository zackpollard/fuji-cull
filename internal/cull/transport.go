package cull

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/zack/fuji-tools/internal/photo"
)

// Transport is a camera link the engine drives without exec'ing anything — iOS
// has neither exec nor usbfs, so the patched aft-mtp-cli cannot run there and
// Apple's ImageCaptureCore (implemented in Swift) provides the link instead.
//
// The link is object-level, not raw PTP, and that is not a style choice:
// ImageCaptureCore's requestSendPTPCommand never delivers a callback on iPadOS
// (verified on 26.5 against the X-H2S — every container encoding, both API
// variants, both threads, before and after the content catalog; see
// internal/ptp for the sweep we would have run). Apple's object API is the
// only door that opens: its content catalog is the index, and partial reads
// ride ICCameraFile's requestReadData, which preserves the engine's chunked
// pull/preempt model unchanged.
//
// The implementation is injected through the gomobile facade as a reverse
// binding, so only gomobile-representable types may cross the boundary: hence
// JSON blobs for listings rather than slices of structs. Implementations must
// serialize their own access — the engine assumes a single-threaded MTP link,
// exactly as with aft.
type Transport interface {
	// Folders lists the camera's media folders as JSON:
	//   [{"dir":"SLOT 1/DCIM/151_FUJI","folder":"151_FUJI"}]
	// `dir` is passed back to Entries; `folder` is the NNN_FUJI display name.
	Folders() ([]byte, error)

	// Entries lists one folder's files as JSON:
	//   [{"objectID":"12","name":"DSCF0001.JPG","size":14379874,"date":"2026-05-10"}]
	Entries(dir string) ([]byte, error)

	// ReadAt reads size bytes at offset from an object (partial read).
	// A short read at the object's tail returns the remaining bytes.
	ReadAt(objectID string, offset, size int64) ([]byte, error)

	// Download pulls a whole object to destPath.
	Download(objectID, destPath string) error

	// Connected reports whether a camera is attached AND enumerated — the
	// host holds this false until its catalog is complete, so Discover never
	// blocks; it just retries until the index is ready.
	Connected() bool
}

// transportFolder / transportEntry mirror the JSON the Transport returns.
type transportFolder struct {
	Dir    string `json:"dir"`
	Folder string `json:"folder"`
}

type transportEntry struct {
	ObjectID string `json:"objectID"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Date     string `json:"date"`
}

// iccBackend is the Backend over a Transport — the ImageCaptureCore path on
// iOS. It mirrors cliBackend, minus everything that needed a subprocess: the
// prefetcher's partial reads go straight to Transport.ReadAt (no serve-parts
// session to open, claim or close), so the single-claim juggling disappears.
type iccBackend struct {
	t Transport
}

func (b *iccBackend) Name() string { return "icc" }

// readAt is the prefetcher's partial-read path (cameraReader).
func (b *iccBackend) readAt(objectID string, offset, size int64) ([]byte, error) {
	return b.t.ReadAt(objectID, offset, size)
}

func (b *iccBackend) Discover(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
	if !b.t.Connected() {
		// The host reports connected only once its catalog is enumerated
		// (~4 min for a 19k-file card), so this is the common path while
		// the camera indexes; the caller retries.
		return nil, fmt.Errorf("waiting for the camera index")
	}
	raw, err := b.t.Folders()
	if err != nil {
		return nil, fmt.Errorf("camera folders: %w", err)
	}
	var folders []transportFolder
	if err := json.Unmarshal(raw, &folders); err != nil {
		return nil, fmt.Errorf("camera folders: %w", err)
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("no NNN_FUJI folders on camera")
	}

	var out []listing
	for _, f := range folders {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		progress(f.Dir, len(out))
		eraw, err := b.t.Entries(f.Dir)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", f.Dir, err)
		}
		var entries []transportEntry
		if err := json.Unmarshal(eraw, &entries); err != nil {
			return nil, fmt.Errorf("list %s: %w", f.Dir, err)
		}
		kept := 0
		for _, e := range entries {
			if _, _, ok := photo.SplitMedia(e.Name); !ok {
				continue
			}
			out = append(out, listing{
				Dir: f.Dir, Folder: f.Folder, Name: e.Name,
				Size: e.Size, Date: captureDay(e.Date), ObjectID: e.ObjectID,
			})
			kept++
		}
		log.Printf("  %s: %d files", f.Dir, kept)
	}
	progress("card index", len(out))
	return out, nil
}

// Fetch is the whole-object fallback; the prefetcher normally pulls images in
// chunks through readAt so demands can preempt cleanly between chunks.
func (b *iccBackend) Fetch(ctx context.Context, items []fetchItem) error {
	for _, it := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if it.ObjectID == "" {
			return fmt.Errorf("no object ID for %s/%s", it.CameraDir, it.Name)
		}
		if err := b.t.Download(it.ObjectID, it.Dest); err != nil {
			return fmt.Errorf("download %s: %w", it.Name, err)
		}
	}
	return nil
}

func (b *iccBackend) LocalPath(s *photo.Shot, ext string) (string, bool) { return "", false }
