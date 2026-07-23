package cull

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"strconv"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/photo"
	"github.com/zack/fuji-tools/internal/ptp"
)

// Transport is a camera link the engine drives without exec'ing anything — iOS
// has neither exec nor usbfs, so the patched aft-mtp-cli cannot run there and
// Apple's ImageCaptureCore (implemented in Swift) provides the link instead.
//
// SendPTP is the primary path: the engine indexes with card-wide
// GetObjectPropList sweeps and reads with GetPartialObject, built and parsed
// in shared Go (internal/ptp) — the same protocol the desktop/Android aft
// patch speaks. The passthrough works on iPadOS behind two app-side gates
// (NSCameraUsageDescription + a control-authorization grant; miss either and
// commands are dropped silently), and queues behind a ~150s ICC-internal
// operation at session open — measured, not documented — after which commands
// interleave fine with ICC's ongoing background crawl (a card-wide filename
// sweep answered in 0.6s mid-crawl). Discover just retries through the block.
//
// The object-level methods are the fallback: ICC's own catalog, complete
// after ~4-5 min, served as JSON listings with reads over requestReadData.
// Used when the sweeps fail.
//
// The implementation is injected through the gomobile facade as a reverse
// binding, so only gomobile-representable types may cross the boundary: hence
// JSON blobs for listings rather than slices of structs. Implementations must
// serialize their own access — the engine assumes a single-threaded MTP link,
// exactly as with aft.
type Transport interface {
	// SendPTP sends a PTP command container (with optional out-data) and
	// returns the data phase.
	SendPTP(command []byte, outData []byte) ([]byte, error)

	// Folders lists the camera's media folders as JSON:
	//   [{"dir":"SLOT 1/DCIM/151_FUJI","folder":"151_FUJI"}]
	// `dir` is passed back to Entries; `folder` is the NNN_FUJI display name.
	Folders() ([]byte, error)

	// Entries lists one folder's files as JSON:
	//   [{"objectID":"12","name":"DSCF0001.JPG","size":14379874,"date":"2026-05-10"}]
	Entries(dir string) ([]byte, error)

	// ReadAt reads size bytes at offset from an object-path object (IDs are
	// "<dir>/<name>"). A short read at the object's tail returns the rest.
	ReadAt(objectID string, offset, size int64) ([]byte, error)

	// Download pulls a whole object-path object to destPath.
	Download(objectID, destPath string) error

	// Connected reports whether a camera session is open.
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
// iOS. It mirrors cliBackend's index (the card-wide "lsprops-all" sweeps),
// minus everything that needed a subprocess: the prefetcher's partial reads
// go straight through the transport (no serve-parts session to open, claim or
// close), so the single-claim juggling disappears.
type iccBackend struct {
	t Transport

	mu       sync.Mutex
	txn      uint32
	objIDs   bool   // last Discover used object-path IDs, not PTP handles
	identity string // "X-H2S 21AQ00123" once DeviceInfo has been parsed
}

// CameraIdentity returns "<model> <serial>" once discovery has parsed
// DeviceInfo ("" before that). Sessions key off it so decisions never bleed
// between cameras.
func (b *iccBackend) CameraIdentity() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.identity
}

func (b *iccBackend) Name() string { return "icc" }

// send issues one PTP command and returns its data phase. MTP is
// single-threaded, so commands are serialized.
func (b *iccBackend) send(cmd func(txn uint32) []byte) ([]byte, error) {
	b.mu.Lock()
	b.txn++
	txn := b.txn
	b.mu.Unlock()
	return b.t.SendPTP(cmd(txn), nil)
}

// propSweep fetches one property for every object on the card.
func (b *iccBackend) propSweep(prop uint16) ([]ptp.PropEntry, error) {
	data, err := b.send(func(txn uint32) []byte { return ptp.PropListAll(prop, txn) })
	if err != nil {
		return nil, err
	}
	return ptp.ParsePropList(data)
}

// readAt is the prefetcher's partial-read path (cameraReader), routed by how
// the index was built: PTP handles read over GetPartialObject, object-path
// IDs over the host's requestReadData.
func (b *iccBackend) readAt(objectID string, offset, size int64) ([]byte, error) {
	b.mu.Lock()
	viaObjects := b.objIDs
	b.mu.Unlock()
	if viaObjects {
		return b.t.ReadAt(objectID, offset, size)
	}
	h, err := strconv.ParseUint(objectID, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("bad object id %q", objectID)
	}
	// GetPartialObject carries 32-bit offset/size; GetPartialObject64 is not
	// implemented by the X-H2S, so this is the same 4 GiB ceiling the desktop
	// build hits (long clips stream a preview and pull in full for the rest).
	if offset+size > 0xFFFFFFFF {
		return nil, fmt.Errorf("32 bit overflow for GetPartialObject")
	}
	return b.send(func(txn uint32) []byte {
		return ptp.PartialObject(uint32(h), uint32(offset), uint32(size), txn)
	})
}

func (b *iccBackend) Discover(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
	if !b.t.Connected() {
		return nil, fmt.Errorf("no camera session")
	}
	out, err := b.discoverPTP(ctx, progress)
	if err == nil {
		b.mu.Lock()
		b.objIDs = false
		b.mu.Unlock()
		return out, nil
	}
	log.Printf("camera: PTP index failed (%v) — falling back to the ICC catalog", err)
	out, oerr := b.discoverObjects(ctx, progress)
	if oerr != nil {
		// surface the PTP error too: the fallback usually just isn't ready yet
		return nil, fmt.Errorf("%w (PTP: %v)", oerr, err)
	}
	b.mu.Lock()
	b.objIDs = true
	b.mu.Unlock()
	return out, nil
}

// discoverPTP indexes the card the way the desktop build does: a card-wide
// GetObjectPropList sweep per property, then the NNN_FUJI folder tree is
// reconstructed from the handle/parent graph.
func (b *iccBackend) discoverPTP(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
	// Probe with a tiny request first: during ICC's ~150s session-start block
	// every command queues, so this times out and the caller retries until
	// the queue drains. A wedged camera (the X-H2S does freeze) also shows up
	// here instead of poisoning a card-wide sweep.
	progress("waiting for the camera link", 0)
	t0 := time.Now()
	info, err := b.send(func(txn uint32) []byte { return ptp.Command(ptp.OpGetDeviceInfo, txn) })
	if err != nil {
		return nil, fmt.Errorf("PTP link probe: %w", err)
	}
	log.Printf("camera: PTP link OK — GetDeviceInfo %d bytes in %s",
		len(info), time.Since(t0).Round(time.Millisecond))
	if di, err := ptp.ParseDeviceInfo(info); err == nil && di.Model != "" {
		b.mu.Lock()
		b.identity = strings.TrimSpace(di.Model + " " + di.Serial)
		b.mu.Unlock()
		log.Printf("camera: %s (serial %s)", di.Model, di.Serial)
	} else {
		// the identity keys the session — a silent parse failure cost a
		// debug cycle once; dump the dataset so it never costs another
		log.Printf("camera: DeviceInfo parse failed (%v, model=%q) — dataset: %x", err, di.Model, info)
	}

	sweep := func(label string, prop uint16) ([]ptp.PropEntry, error) {
		progress("reading card index: "+label, 0)
		t := time.Now()
		e, err := b.propSweep(prop)
		if err != nil {
			log.Printf("camera: %s sweep failed after %s: %v", label, time.Since(t).Round(time.Millisecond), err)
			return nil, err
		}
		log.Printf("camera: %s — %d entries in %s", label, len(e), time.Since(t).Round(time.Millisecond))
		return e, nil
	}

	names, err := sweep("filenames", ptp.PropFileName)
	if err != nil {
		return nil, fmt.Errorf("card index (filenames): %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("card index returned no objects")
	}
	nameOf := make(map[uint32]string, len(names))
	for _, e := range names {
		if e.IsStr {
			nameOf[e.Handle] = e.Str
		}
	}
	progress("reading card index", len(nameOf))

	parents, err := sweep("parents", ptp.PropParentObject)
	if err != nil {
		return nil, fmt.Errorf("card index (parents): %w", err)
	}
	parentOf := make(map[uint32]uint32, len(parents))
	for _, e := range parents {
		parentOf[e.Handle] = uint32(e.Num)
	}

	sizes, err := sweep("sizes", ptp.PropObjectSize)
	if err != nil {
		return nil, fmt.Errorf("card index (sizes): %w", err)
	}
	sizeOf := make(map[uint32]int64, len(sizes))
	for _, e := range sizes {
		sizeOf[e.Handle] = int64(e.Num)
	}

	// Capture dates drive the timeline grouping; best-effort, some devices
	// refuse the property card-wide.
	dayOf := map[uint32]string{}
	if dates, err := sweep("dates", ptp.PropDateCreated); err == nil {
		for _, e := range dates {
			if e.IsStr {
				dayOf[e.Handle] = ptp.CaptureDay(e.Str)
			}
		}
	} else {
		log.Printf("card index: capture dates unavailable (%v) — grouping by folder", err)
	}

	// Camera folders are the NNN_FUJI buckets; the path is their ancestry.
	pathOf := func(h uint32) string {
		parts := []string{}
		for cur, hops := h, 0; cur != 0 && hops < 16; hops++ {
			n, ok := nameOf[cur]
			if !ok || n == "" {
				break
			}
			parts = append([]string{n}, parts...)
			cur = parentOf[cur]
		}
		out := ""
		for i, p := range parts {
			if i > 0 {
				out += "/"
			}
			out += p
		}
		return out
	}

	folders := map[uint32]string{} // handle -> NNN_FUJI name
	for h, n := range nameOf {
		if photo.FolderRe.MatchString(n) {
			folders[h] = n
		}
	}
	if len(folders) == 0 {
		sample := make([]string, 0, 12)
		for _, n := range nameOf {
			sample = append(sample, n)
			if len(sample) == 12 {
				break
			}
		}
		sort.Strings(sample)
		return nil, fmt.Errorf("no NNN_FUJI folders among %d objects (sample: %v)", len(nameOf), sample)
	}

	byFolder := map[uint32][]uint32{}
	for h := range nameOf {
		if p, ok := parentOf[h]; ok {
			if _, isFolder := folders[p]; isFolder {
				byFolder[p] = append(byFolder[p], h)
			}
		}
	}

	folderHandles := make([]uint32, 0, len(folders))
	for h := range folders {
		folderHandles = append(folderHandles, h)
	}
	sort.Slice(folderHandles, func(i, j int) bool { return folders[folderHandles[i]] < folders[folderHandles[j]] })

	var out []listing
	for _, fh := range folderHandles {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dir := pathOf(fh)
		kids := byFolder[fh]
		sort.Slice(kids, func(i, j int) bool { return nameOf[kids[i]] < nameOf[kids[j]] })
		kept := 0
		for _, h := range kids {
			name := nameOf[h]
			if _, _, ok := photo.SplitMedia(name); !ok {
				continue
			}
			out = append(out, listing{
				Dir: dir, Folder: folders[fh], Name: name,
				Size: sizeOf[h], Date: dayOf[h],
				ObjectID: strconv.FormatUint(uint64(h), 10),
			})
			kept++
		}
		log.Printf("  %s: %d files", dir, kept)
		progress(dir, len(out))
	}
	return out, nil
}

// discoverObjects reads the index from ICC's own completed catalog.
func (b *iccBackend) discoverObjects(ctx context.Context, progress func(stage string, files int)) ([]listing, error) {
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
	b.mu.Lock()
	viaObjects := b.objIDs
	b.mu.Unlock()
	for _, it := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if it.ObjectID == "" {
			return fmt.Errorf("no object ID for %s/%s", it.CameraDir, it.Name)
		}
		if viaObjects {
			if err := b.t.Download(it.ObjectID, it.Dest); err != nil {
				return fmt.Errorf("download %s: %w", it.Name, err)
			}
			continue
		}
		if err := b.downloadPTP(ctx, it); err != nil {
			return fmt.Errorf("download %s: %w", it.Name, err)
		}
	}
	return nil
}

// downloadPTP pulls a whole object through chunked GetPartialObject reads.
func (b *iccBackend) downloadPTP(ctx context.Context, it fetchItem) error {
	const chunk = 8 << 20
	if err := os.MkdirAll(filepath.Dir(it.Dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(it.Dest)
	if err != nil {
		return err
	}
	defer f.Close()
	var off int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, err := b.readAt(it.ObjectID, off, chunk)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return nil
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
		off += int64(len(data))
		if len(data) < chunk {
			return nil
		}
	}
}

func (b *iccBackend) LocalPath(s *photo.Shot, ext string) (string, bool) { return "", false }
