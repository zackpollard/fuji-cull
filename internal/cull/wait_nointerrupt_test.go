package cull

import (
	"context"
	"sync"
	"testing"

	"github.com/zack/fuji-tools/internal/photo"
)

func waitFixture(id string) (*Prefetcher, *bool) {
	s := &photo.Shot{ID: id, Kind: "photo", Base: "DSCF0001",
		CameraDir: "SLOT 1/DCIM/151_FUJI", Files: map[string]string{"JPG": "DSCF0001.JPG"}}
	p := &Prefetcher{
		cat:    &Catalog{Shots: []*photo.Shot{s}, Index: map[string]int{id: 0}},
		state:  map[string]*fetchState{},
		demand: map[string]bool{},
	}
	p.cond = sync.NewCond(&p.mu)
	cancelled := false
	p.imgCancel = func() { cancelled = true }
	return p, &cancelled
}

// Asking for a shot that is already in the buffer must not disturb the camera.
// It used to cancel the in-flight fetch batch, and with a decode pool asking
// per frame that meant no batch ever finished and the buffer drained.
func TestWaitOnBufferedShotLeavesTheCameraAlone(t *testing.T) {
	const id = "SLOT 1/DCIM/151_FUJI/DSCF0001"
	p, cancelled := waitFixture(id)
	p.state[id] = &fetchState{Status: "ready"}

	path, err := p.Wait(context.Background(), p.cat.Shots[0].ID)
	if err != nil {
		t.Fatalf("Wait on a ready shot: %v", err)
	}
	if path == "" {
		t.Error("Wait returned an empty path for a ready shot")
	}
	if *cancelled {
		t.Error("Wait cancelled the in-flight fetch batch for a shot already on disk")
	}
	if len(p.demand) != 0 {
		t.Errorf("Wait raised a demand for a buffered shot: %v", p.demand)
	}
}

// A shot that is NOT buffered still preempts: that is what makes a jump past
// the buffer edge fetch immediately instead of queueing behind the window.
func TestWaitOnMissingShotStillPreempts(t *testing.T) {
	const id = "SLOT 1/DCIM/151_FUJI/DSCF0001"
	p, cancelled := waitFixture(id)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // return as soon as the preemption has been done
	_, _ = p.Wait(ctx, id)

	if !*cancelled {
		t.Error("Wait did not preempt the in-flight batch for an unbuffered shot")
	}
}
