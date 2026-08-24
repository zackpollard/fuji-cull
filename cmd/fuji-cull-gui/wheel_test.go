package main

import (
	"math"
	"testing"

	"github.com/veandco/go-sdl2/sdl"
)

func TestWheelDelta(t *testing.T) {
	cases := []struct {
		name string
		ev   sdl.MouseWheelEvent
		want float64
		ok   bool
	}{
		{"detent up", sdl.MouseWheelEvent{Y: 1, PreciseY: 1}, 1, true},
		{"detent down", sdl.MouseWheelEvent{Y: -1, PreciseY: -1}, -1, true},
		// sdl2-compat rounds a real -1.0 scroll to y=0; PreciseY must win, or
		// the event is dropped and the view does not move at all
		{"integer rounded to zero", sdl.MouseWheelEvent{Y: 0, PreciseY: -1}, -1, true},
		{"no precise value", sdl.MouseWheelEvent{Y: -2, PreciseY: 0}, -2, true},
		{"flipped", sdl.MouseWheelEvent{Y: 1, PreciseY: 1, Direction: sdl.MOUSEWHEEL_FLIPPED}, -1, true},
		// an ordinary trackball sweep, which must still come through
		{"trackball sweep", sdl.MouseWheelEvent{Y: -13, PreciseY: -13.1}, -13.1, true},
		// the mis-scaled duplicate stream, captured live: applying one of these
		// threw the zoom across its whole range in a single frame
		{"spurious 1138", sdl.MouseWheelEvent{Y: 1138, PreciseY: 1138}, 0, false},
		{"spurious 1481", sdl.MouseWheelEvent{Y: -1481, PreciseY: -1481}, 0, false},
	}
	for _, c := range cases {
		got, ok := wheelDelta(&c.ev)
		if ok != c.ok || (c.ok && math.Abs(got-c.want) > 1e-4) {
			t.Errorf("%s: wheelDelta = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// The device reports every 10ms with a magnitude around 15. Unlimited, that is
// 1.15^1560 per second — the zoom pinned against a stop instantly. What matters
// is that travel is bounded by TIME rather than by how many events the device
// chose to send; the exact speed is taste, set by zoomClicksPerSec.
func TestZoomIsRateLimited(t *testing.T) {
	scale := 1.0
	for i := 0; i < 100; i++ { // one second of continuous scrolling
		scale *= zoomFactor(wheelStep(15.6, 0.01, zoomClicksPerSec))
	}
	// the same second unlimited would be ~1e97
	if scale > 1e4 {
		t.Errorf("one second of scrolling zoomed %.3gx — not bounded", scale)
	}
	if scale < 4 {
		t.Errorf("one second of scrolling zoomed only %.1fx — sluggish", scale)
	}
	// and a short flick should still do something useful
	flick := 1.0
	for i := 0; i < 15; i++ { // 0.15s
		flick *= zoomFactor(wheelStep(15.6, 0.01, zoomClicksPerSec))
	}
	if flick < 1.5 {
		t.Errorf("a 0.15s flick zoomed %.2fx, want it to feel responsive", flick)
	}
}

// The rate limit exists for bursts and must not make an ordinary wheel feel
// dead: an isolated detent applies in full.
func TestIsolatedDetentIsUnthrottled(t *testing.T) {
	if got := wheelStep(1, 0.5, zoomClicksPerSec); got != 1 {
		t.Errorf("isolated detent throttled to %v, want the full 1", got)
	}
	// ...but a trackball sweep opening after a pause must not lurch
	if got := wheelStep(13.1, 0.5, zoomClicksPerSec); got != 1 {
		t.Errorf("post-pause burst applied %v clicks, want it held to 1", got)
	}
}

// The old factor (1 + 0.15*dy) hit zero at dy=-6.67 and went negative past it,
// so a burst zoomed the wrong way and snapped the image back to fit.
func TestZoomFactor(t *testing.T) {
	for dy := -50.0; dy <= 50; dy += 0.1 {
		if f := zoomFactor(dy); f <= 0 {
			t.Fatalf("zoomFactor(%v) = %v, must stay positive", dy, f)
		}
	}
	if f := zoomFactor(1); math.Abs(f-1.15) > 1e-9 {
		t.Errorf("zoomFactor(1) = %v, want 1.15", f)
	}
	if round := zoomFactor(1) * zoomFactor(-1); math.Abs(round-1) > 1e-9 {
		t.Errorf("zoom in*out = %v, want 1", round)
	}
	if zoomFactor(-1) >= 1 || zoomFactor(1) <= 1 {
		t.Error("zoom direction inverted")
	}
}

// The captured failure, end to end: a run of zoom-out events followed by one
// event from the mis-scaled stream. Before the fix that last event multiplied
// the scale by 171 and slammed the image to the zoom stop.
func TestSpuriousEventCannotReverseAZoomOut(t *testing.T) {
	scale := 4.0
	apply := func(ev sdl.MouseWheelEvent, dt float64) {
		dy, ok := wheelDelta(&ev)
		if !ok {
			return
		}
		scale *= zoomFactor(wheelStep(dy, dt, zoomClicksPerSec))
	}
	for i := 0; i < 20; i++ {
		apply(sdl.MouseWheelEvent{Y: -13, PreciseY: -13.1}, 0.01)
	}
	zoomedOut := scale
	if zoomedOut >= 4 {
		t.Fatalf("scrolling out did not zoom out: %v", zoomedOut)
	}
	apply(sdl.MouseWheelEvent{Y: 1138, PreciseY: 1138}, 1.5) // the spurious one
	if scale != zoomedOut {
		t.Errorf("spurious event moved zoom %v -> %v, want it ignored", zoomedOut, scale)
	}
}
