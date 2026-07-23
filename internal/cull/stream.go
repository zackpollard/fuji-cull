package cull

import (
	"fmt"
	"io"
	"log"
	"time"

	"github.com/zack/fuji-tools/internal/mtppart"
	"github.com/zack/fuji-tools/internal/photo"
)

// Camera video streaming: videos play without pulling the whole file by
// serving HTTP ranges straight off the camera through one persistent
// serve-parts session. The session owns the MTP claim, so the prefetcher is
// paused (refcounted) while a stream is open and resumed by the janitor once
// no range has been read for streamIdle. Fuji writes moov at the head, so
// players start after the first chunk.
const (
	streamChunk    = 8 << 20 // camera read granularity (link runs ~55 MB/s)
	streamChunkMax = 8       // LRU chunks held (~64 MB); seeks re-read
	streamAhead    = 2       // background read-ahead depth beyond the reader
	streamIdle     = 20 * time.Second
)

// streamPartialLimit caps camera streaming at MTP GetPartialObject's 32-bit
// offset ceiling. The X-H2S lacks GetPartialObject64 (0x95C1) — verified in the
// field: reads past 4 GiB return "32 bit overflow" — so a clip longer than this
// cannot be partial-read past the ceiling and would freeze mid-playback. We
// expose only the streamable prefix as a preview (chunk-aligned, with headroom
// so the final chunk's offset+size stays under 2^32) and the UI directs the
// user to a full local pull (GetObject has no such limit) to watch the rest.
const streamPartialLimit = 510 * streamChunk // 4080 MiB, safely below 2^32

// streamSize is the byte length streamable off the camera for this file: the
// whole file, or the preview prefix when it exceeds the partial-read ceiling.
func streamSize(s *photo.Shot, ext string) int64 {
	if sz := s.Sizes[ext]; sz <= streamPartialLimit {
		return sz
	}
	return streamPartialLimit
}

// StreamLimited reports whether a video exceeds the partial-read ceiling, so
// only a streamed preview is available and the full clip needs a local pull.
func (p *Prefetcher) StreamLimited(s *photo.Shot, ext string) bool {
	return s != nil && s.Sizes[ext] > streamPartialLimit
}

// streamSource is the byte source a video stream reads from: the aft
// serve-parts subprocess on desktop/Android, the camera transport on iOS
// (which has no exec). *mtppart.Server satisfies it as-is.
type streamSource interface {
	ReadAt(objectID string, offset, size int64) ([]byte, error)
	Close()
}

// cameraStreamSource streams through the injected Transport. There is no
// session to tear down — the host owns the link — so Close is a no-op.
type cameraStreamSource struct{ c cameraReader }

func (s cameraStreamSource) ReadAt(objectID string, offset, size int64) ([]byte, error) {
	return s.c.readAt(objectID, offset, size)
}
func (s cameraStreamSource) Close() {}

type streamState struct {
	srv       streamSource
	shotID    string
	objID     string
	size      int64
	last      time.Time
	chunks    map[int64][]byte
	inflight  map[int64]chan struct{} // chunk fetches in progress (streamMu released during camera IO)
	order     []int64
	lastRead  int64 // chunk index the player most recently consumed
	readahead bool  // a background read-ahead goroutine is active
}

// StreamingAvailable reports whether camera streaming works at all right now
// (partial-read binary present and not tripped).
func (p *Prefetcher) StreamingAvailable() bool {
	if !p.partsOK() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.partSick
}

// CanStream reports whether a shot's video could be streamed right now.
func (p *Prefetcher) CanStream(s *photo.Shot, ext string) bool {
	if s == nil || s.Kind != "video" || !p.partsOK() {
		return false
	}
	p.mu.Lock()
	sick := p.partSick
	p.mu.Unlock()
	return !sick && s.ObjectIDs[ext] != "" && s.Sizes[ext] > 0
}

// StreamReader returns a ReadSeeker over the video's bytes on camera,
// suitable for http.ServeContent. Opening (or switching shots) pauses the
// prefetcher and claims the device; reads keep the session alive.
func (p *Prefetcher) StreamReader(s *photo.Shot, ext string) (io.ReadSeeker, error) {
	p.streamMu.Lock()
	defer p.streamMu.Unlock()
	if err := p.ensureStreamLocked(s, ext); err != nil {
		return nil, err
	}
	return io.NewSectionReader(&streamReaderAt{p: p, shotID: s.ID, ext: ext, s: s}, 0, streamSize(s, ext)), nil
}

// ensureStreamLocked opens (or re-targets) the serve-parts session and
// validates the head chunk, tripping the partial-read breaker on garbage.
func (p *Prefetcher) ensureStreamLocked(s *photo.Shot, ext string) error {
	if st := p.stream; st != nil && st.shotID == s.ID {
		return nil
	}
	p.closeStreamLocked()
	if !p.CanStream(s, ext) {
		return fmt.Errorf("camera streaming unavailable")
	}
	p.PauseAndDrain()
	// desktop/Android open a serve-parts subprocess; iOS reads through the
	// camera transport it already holds
	var src streamSource
	if p.partBin != "" {
		srv, err := mtppart.StartServer()
		if err != nil {
			p.Resume()
			return err
		}
		src = srv
	} else {
		src = cameraStreamSource{p.camera}
	}
	st := &streamState{
		srv: src, shotID: s.ID, objID: s.ObjectIDs[ext],
		size: streamSize(s, ext), last: time.Now(),
		chunks: map[int64][]byte{}, inflight: map[int64]chan struct{}{},
	}
	p.stream = st // claim before the head fetch: streamFetch drops the lock
	head, err := p.streamFetch(st, 0)
	if err != nil {
		p.closeStreamLocked()
		return err
	}
	if len(head) < 8 || string(head[4:8]) != "ftyp" {
		p.closeStreamLocked()
		p.mu.Lock()
		p.markPartSickLocked()
		p.mu.Unlock()
		return fmt.Errorf("camera returned stale-buffer garbage for %s — power-cycle the camera", s.ID)
	}
	log.Printf("stream: %s open (%.1f MB on camera)", s.ID, float64(st.size)/(1<<20))
	return nil
}

// streamFetch returns one chunk, fetching from the camera on miss. Call with
// streamMu held; the lock is RELEASED during the camera transfer (serve-parts
// serializes internally) so cached reads and the response writer never stall
// behind camera IO. Concurrent requests for the same chunk wait, not refetch.
func (p *Prefetcher) streamFetch(st *streamState, idx int64) ([]byte, error) {
	for {
		if data, ok := st.chunks[idx]; ok {
			return data, nil
		}
		ch, fetching := st.inflight[idx]
		if fetching {
			p.streamMu.Unlock()
			<-ch
			p.streamMu.Lock()
			if p.stream != st {
				return nil, fmt.Errorf("stream closed")
			}
			continue
		}
		ch = make(chan struct{})
		st.inflight[idx] = ch
		off := idx * streamChunk
		want := int64(streamChunk)
		if off+want > st.size {
			want = st.size - off
		}
		p.streamMu.Unlock()
		t0 := time.Now()
		data, err := st.srv.ReadAt(st.objID, off, want)
		if d := time.Since(t0); err == nil {
			// throughput is THE camera-streaming viability number (aft runs
			// ~55 MB/s; a slow transport shows up here as a playback stall)
			log.Printf("stream: chunk %d — %.1f MB in %s (%.1f MB/s)",
				idx, float64(len(data))/(1<<20), d.Round(time.Millisecond),
				float64(len(data))/(1<<20)/d.Seconds())
		}
		p.streamMu.Lock()
		delete(st.inflight, idx)
		close(ch)
		if p.stream != st {
			return nil, fmt.Errorf("stream closed")
		}
		if err != nil {
			return nil, err
		}
		st.chunks[idx] = data
		st.order = append(st.order, idx)
		for len(st.order) > streamChunkMax {
			delete(st.chunks, st.order[0])
			st.order = st.order[1:]
		}
		return data, nil
	}
}

// scheduleReadahead pipelines upcoming chunks while the player decodes the
// current one — without it every chunk boundary stalls playback for the full
// camera fetch, and high-bitrate clips have no headroom for that. Bounded to
// streamAhead chunks past the reader so it can never race ahead and evict
// what the player needs next. Call with streamMu held.
func (p *Prefetcher) scheduleReadahead(shotID string, idx int64) {
	st := p.stream
	if st == nil || st.shotID != shotID || st.readahead {
		return
	}
	// first missing chunk within depth of the reader
	for ; idx <= st.lastRead+streamAhead && idx*streamChunk < st.size; idx++ {
		if _, ok := st.chunks[idx]; !ok {
			break
		}
	}
	if idx > st.lastRead+streamAhead || idx*streamChunk >= st.size {
		return
	}
	st.readahead = true
	go func(idx int64) {
		p.streamMu.Lock()
		defer p.streamMu.Unlock()
		st := p.stream
		if st == nil || st.shotID != shotID {
			return
		}
		st.readahead = false
		if _, err := p.streamFetch(st, idx); err == nil {
			p.scheduleReadahead(shotID, idx+1)
		}
	}(idx)
}

type streamReaderAt struct {
	p      *Prefetcher
	shotID string
	ext    string
	s      *photo.Shot
}

func (r *streamReaderAt) ReadAt(b []byte, off int64) (int, error) {
	r.p.streamMu.Lock()
	defer r.p.streamMu.Unlock()
	// The janitor may have released the session between requests (or another
	// video took it); transparently reopen for this shot.
	if err := r.p.ensureStreamLocked(r.s, r.ext); err != nil {
		return 0, err
	}
	st := r.p.stream
	st.last = time.Now()
	total := 0
	for total < len(b) {
		off2 := off + int64(total)
		if off2 >= st.size {
			return total, io.EOF
		}
		idx := off2 / streamChunk
		data, err := r.p.streamFetch(st, idx)
		if err != nil {
			return total, err
		}
		st.lastRead = idx // re-anchors read-ahead after seeks in either direction
		inChunk := off2 % streamChunk
		if inChunk >= int64(len(data)) {
			return total, io.EOF
		}
		total += copy(b[total:], data[inChunk:])
	}
	r.p.scheduleReadahead(r.shotID, off/streamChunk+1)
	return total, nil
}

// streamJanitor releases the camera once a stream has gone idle so
// prefetching and sweeps resume without the player having to say goodbye.
func (p *Prefetcher) streamJanitor() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for range tick.C {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		p.streamMu.Lock()
		if st := p.stream; st != nil && (closed || time.Since(st.last) > streamIdle) {
			log.Printf("stream: %s idle, releasing camera", st.shotID)
			p.closeStreamLocked()
		}
		p.streamMu.Unlock()
		if closed {
			return
		}
	}
}

// CloseStream releases any live stream session immediately — an explicit
// video pull needs the camera link the stream is holding paused.
func (p *Prefetcher) CloseStream() {
	p.streamMu.Lock()
	p.closeStreamLocked()
	p.streamMu.Unlock()
}

// closeStreamLocked tears down the session and resumes the prefetcher
// (call with streamMu held).
func (p *Prefetcher) closeStreamLocked() {
	if p.stream == nil {
		return
	}
	p.stream.srv.Close()
	p.stream = nil
	p.Resume()
}

// closeStreamIfElsewhere releases a live camera video stream once the cursor
// has moved off its shot, resuming the prefetcher immediately instead of after
// the janitor's streamIdle timeout (the "photos won't load right after a video"
// stall). It reads the CURRENT cursor rather than a value captured when the
// call was scheduled: SetCursor spawns this async, so a stale navigation could
// otherwise tear down the stream a later navigation legitimately opened — which
// showed up as a video failing to load on the first tab onto it. Safe to call
// without p.mu held: it takes p.mu briefly, then streamMu, and
// closeStreamLocked's Resume re-takes p.mu (lock order streamMu → p.mu).
func (p *Prefetcher) closeStreamIfElsewhere() {
	p.mu.Lock()
	var cursorID string
	if p.cursor >= 0 && p.cursor < len(p.cat.Shots) {
		cursorID = p.cat.Shots[p.cursor].ID
	}
	p.mu.Unlock()
	p.streamMu.Lock()
	if st := p.stream; st != nil && st.shotID != cursorID {
		log.Printf("stream: %s left, releasing camera (cursor moved)", st.shotID)
		p.closeStreamLocked()
	}
	p.streamMu.Unlock()
}
