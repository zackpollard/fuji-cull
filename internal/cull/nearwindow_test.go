package cull

import (
	"strconv"
	"testing"

	"github.com/zack/fuji-tools/internal/photo"
)

func thinFixture(n int) *Prefetcher {
	shots := make([]*photo.Shot, n)
	for i := range shots {
		shots[i] = &photo.Shot{ID: strconv.Itoa(i), Kind: "photo"}
	}
	return &Prefetcher{
		cat:    &Catalog{Shots: shots},
		ahead:  150,
		cursor: 0,
		state:  map[string]*fetchState{},
	}
}

// fill marks shots [from,to) with a status.
func fill(p *Prefetcher, from, to int, status string) {
	for i := from; i < to; i++ {
		p.state[strconv.Itoa(i)] = &fetchState{Status: status}
	}
}

func TestNearWindowThin(t *testing.T) {
	t.Run("covered ground is not thin", func(t *testing.T) {
		p := thinFixture(300)
		fill(p, 1, nearWindow+1, "ready")
		if p.nearWindowThinLocked() {
			t.Error("reported thin with every near shot buffered")
		}
	})

	t.Run("a hole just ahead is thin", func(t *testing.T) {
		p := thinFixture(300)
		fill(p, 1, nearWindow+1, "ready")
		delete(p.state, strconv.Itoa(nearWindow-3))
		if !p.nearWindowThinLocked() {
			t.Error("missed a hole inside the near window")
		}
	})

	// Beyond the near window the head sweep may still take its turn: that is
	// the fairness the grid depends on, and it is far enough off that a fetch
	// batch lands before the cursor reaches it.
	t.Run("a hole far ahead is not thin", func(t *testing.T) {
		p := thinFixture(300)
		fill(p, 1, nearWindow+1, "ready")
		if p.nearWindowThinLocked() {
			t.Error("a hole beyond the near window counted as thin")
		}
	})

	// In-flight and failed shots are already being dealt with; treating them
	// as holes would starve the head sweep permanently on a card with a shot
	// the camera cannot serve.
	t.Run("in-flight and failed are not holes", func(t *testing.T) {
		p := thinFixture(300)
		fill(p, 1, nearWindow+1, "ready")
		p.state[strconv.Itoa(3)] = &fetchState{Status: "fetching"}
		p.state[strconv.Itoa(4)] = &fetchState{Status: "failed"}
		if p.nearWindowThinLocked() {
			t.Error("an in-flight or failed shot counted as a hole")
		}
	})

	// Videos are fetched on demand, never by the window, so an unfetched one
	// ahead of the cursor is not a hole the window should chase.
	t.Run("videos are not holes", func(t *testing.T) {
		p := thinFixture(300)
		fill(p, 1, nearWindow+1, "ready")
		delete(p.state, "5")
		p.cat.Shots[5].Kind = "video"
		if p.nearWindowThinLocked() {
			t.Error("an unfetched video counted as a hole")
		}
	})

	// Near the end of the card there is nothing left to be thin about.
	t.Run("end of card", func(t *testing.T) {
		p := thinFixture(5)
		fill(p, 1, 5, "ready")
		if p.nearWindowThinLocked() {
			t.Error("reported thin at the end of the card")
		}
	})
}
