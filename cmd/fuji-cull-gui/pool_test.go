package main

import (
	"context"
	"sync"
	"testing"
)

// fakeSource reports a fixed set of shots as buffered; everything else blocks
// in WaitImage the way a real camera fetch does.
type fakeSource struct {
	mu    sync.Mutex
	ready map[string]bool
	waits int // WaitImage calls that had to block
}

func (f *fakeSource) ImagePathIfReady(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready[id] {
		return "/buffered/" + id, true
	}
	return "", false
}

func (f *fakeSource) WaitImage(ctx context.Context, id string) (string, error) {
	f.mu.Lock()
	if !f.ready[id] {
		f.waits++
	}
	f.mu.Unlock()
	if _, ok := f.ImagePathIfReady(id); ok {
		return "/buffered/" + id, nil
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func newTestPool(src imageSource, want []string) *decodePool {
	return &decodePool{
		app: src, want: want, boxW: 3840, boxH: 2160,
		inflight: map[string]context.CancelFunc{},
		done:     map[string]*decoded{},
		waiters:  map[string]bool{},
	}
}

// A worker must never sit blocked on the camera while frames that are already
// in the buffer wait to be decoded — that is a full buffer that still feels
// empty, and it is what pinned every worker before.
func TestNextPrefersBufferedOverCameraFetch(t *testing.T) {
	src := &fakeSource{ready: map[string]bool{"c": true, "d": true}}
	p := newTestPool(src, []string{"a", "b", "c", "d"}) // a, b are still on the camera

	// "a" is the cursor's shot, so it earns the single blocking slot.
	j1, ok := p.next()
	if !ok || j1.id != "a" || !j1.waits {
		t.Fatalf("first job = %+v ok=%v, want a blocking fetch of the cursor shot", j1, ok)
	}
	// Everything after it must be a buffered frame, decoded straight away.
	for i := 0; i < 2; i++ {
		j, ok := p.next()
		if !ok {
			t.Fatalf("job %d: pool went idle with buffered frames waiting", i+2)
		}
		if j.waits {
			t.Errorf("job %d = %q blocks on the camera; only the cursor shot may", i+2, j.id)
		}
		if j.id != "c" && j.id != "d" {
			t.Errorf("job %d = %q, want one of the buffered frames c/d", i+2, j.id)
		}
	}
	// "b" is neither buffered nor the cursor shot, so nothing is left to do.
	if j, ok := p.next(); ok {
		t.Errorf("pool handed out %+v; expected it to idle rather than block on %q", j, j.id)
	}
}

// Only one worker may block, so eighteen of them cannot each raise a demand
// and cancel one another's fetch batch.
func TestNextCapsBlockingWorkers(t *testing.T) {
	src := &fakeSource{ready: map[string]bool{}}
	p := newTestPool(src, []string{"a", "b", "c"})

	if j, ok := p.next(); !ok || !j.waits {
		t.Fatalf("first job = %+v ok=%v, want a blocking fetch", j, ok)
	}
	for i := 0; i < 3; i++ {
		if j, ok := p.next(); ok {
			t.Fatalf("pool handed out a second blocking job %+v; cap is %d", j, maxFetchWaiters)
		}
	}
}

// A frame decoded to fit is re-decoded only when something needs every pixel.
func TestNextUpgradesOnlyTheFullWant(t *testing.T) {
	src := &fakeSource{ready: map[string]bool{"a": true, "b": true}}
	p := newTestPool(src, []string{"a", "b"})
	p.done["a"] = &decoded{full: false}
	p.done["b"] = &decoded{full: false}

	if j, ok := p.next(); ok {
		t.Fatalf("fitted frames re-decoded with nothing asking for full size: %+v", j)
	}
	p.fullWant = "b"
	j, ok := p.next()
	if !ok || j.id != "b" || !j.full {
		t.Fatalf("job = %+v ok=%v, want a full-size re-decode of b", j, ok)
	}
}

// Once the cursor moves off the shot a worker is blocked on, that worker is
// released — it is worth more decoding what has landed than holding a place.
func TestSetWantReleasesStaleWaiter(t *testing.T) {
	src := &fakeSource{ready: map[string]bool{}}
	p := newTestPool(src, []string{"a", "b", "c"})
	j, ok := p.next()
	if !ok || !j.waits {
		t.Fatalf("first job = %+v ok=%v, want a blocking fetch of a", j, ok)
	}
	p.SetWant([]string{"b", "c", "a"}) // cursor moved to b
	if j.ctx.Err() == nil {
		t.Error("worker still blocked on a after the cursor moved to b")
	}
}
