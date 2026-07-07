// Package mpvsw embeds mpv via libmpv's software-render API: mpv decodes
// (with hardware acceleration where available) and hands us RGBA frames to
// upload as textures. This is the only embedding path that works under
// Wayland (no foreign window embedding) and it keeps audio, seeking and
// HEVC support without an external player window.
package mpvsw

/*
#cgo LDFLAGS: -lmpv
#include <stdlib.h>
#include <mpv/client.h>
#include <mpv/render.h>

static int cmd2(mpv_handle *h, const char *a, const char *b) {
	const char *args[] = {a, b, NULL};
	return mpv_command(h, args);
}
static int cmd1(mpv_handle *h, const char *a) {
	const char *args[] = {a, NULL};
	return mpv_command(h, args);
}
static mpv_render_context *create_sw(mpv_handle *h) {
	mpv_render_context *rc = NULL;
	mpv_render_param params[] = {
		{MPV_RENDER_PARAM_API_TYPE, (void *)MPV_RENDER_API_TYPE_SW},
		{0, NULL},
	};
	if (mpv_render_context_create(&rc, h, params) < 0) return NULL;
	return rc;
}
static int render_sw(mpv_render_context *rc, int w, int h, void *buf) {
	int size[2] = {w, h};
	size_t stride = (size_t)w * 4;
	mpv_render_param params[] = {
		{MPV_RENDER_PARAM_SW_SIZE, size},
		{MPV_RENDER_PARAM_SW_FORMAT, (void *)"bgra"},
		{MPV_RENDER_PARAM_SW_STRIDE, &stride},
		{MPV_RENDER_PARAM_SW_POINTER, buf},
		{0, NULL},
	};
	return mpv_render_context_render(rc, params);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Player is a single embedded mpv instance reused across videos.
type Player struct {
	h   *C.mpv_handle
	rc  *C.mpv_render_context
	buf []byte
	w   int
	hgt int
}

func New() (*Player, error) {
	h := C.mpv_create()
	if h == nil {
		return nil, fmt.Errorf("mpv_create failed")
	}
	set := func(k, v string) {
		ck, cv := C.CString(k), C.CString(v)
		C.mpv_set_option_string(h, ck, cv)
		C.free(unsafe.Pointer(ck))
		C.free(unsafe.Pointer(cv))
	}
	set("vo", "libmpv")
	// auto-copy: hardware decode with copy-back — the SW render context
	// cannot consume non-copyback hwdec frames, and mpv's silent fallback is
	// software-decoding 4K 10-bit HEVC (very laggy).
	set("hwdec", "auto-copy")
	// fast scaling paths for the software renderer; per-frame swscale at
	// high quality costs more than the decode itself.
	set("sw-fast", "yes")
	set("keep-open", "yes")
	set("audio-client-name", "fuji-cull")
	if C.mpv_initialize(h) < 0 {
		C.mpv_destroy(h)
		return nil, fmt.Errorf("mpv_initialize failed")
	}
	rc := C.create_sw(h)
	if rc == nil {
		C.mpv_destroy(h)
		return nil, fmt.Errorf("mpv render context (sw) failed")
	}
	return &Player{h: h, rc: rc}, nil
}

// Load starts playing a file (path or URL).
func (p *Player) Load(target string) {
	ct := C.CString(target)
	defer C.free(unsafe.Pointer(ct))
	cl := C.CString("loadfile")
	defer C.free(unsafe.Pointer(cl))
	C.cmd2(p.h, cl, ct)
	p.SetPause(false)
}

func (p *Player) Stop() {
	cs := C.CString("stop")
	defer C.free(unsafe.Pointer(cs))
	C.cmd1(p.h, cs)
}

func (p *Player) SetPause(paused bool) {
	v := C.CString("no")
	if paused {
		v = C.CString("yes")
	}
	k := C.CString("pause")
	C.mpv_set_property_string(p.h, k, v)
	C.free(unsafe.Pointer(k))
	C.free(unsafe.Pointer(v))
}

func (p *Player) Paused() bool {
	k := C.CString("pause")
	defer C.free(unsafe.Pointer(k))
	s := C.mpv_get_property_string(p.h, k)
	if s == nil {
		return false
	}
	defer C.mpv_free(unsafe.Pointer(s))
	return C.GoString(s) == "yes"
}

// Seek jumps relative seconds.
func (p *Player) Seek(secs float64) {
	ck := C.CString("seek")
	cv := C.CString(fmt.Sprintf("%f", secs))
	C.cmd2(p.h, ck, cv)
	C.free(unsafe.Pointer(ck))
	C.free(unsafe.Pointer(cv))
}

// Position returns playback position and duration in seconds.
func (p *Player) Position() (pos, dur float64) {
	get := func(name string) float64 {
		cn := C.CString(name)
		defer C.free(unsafe.Pointer(cn))
		var d C.double
		if C.mpv_get_property(p.h, cn, C.MPV_FORMAT_DOUBLE, unsafe.Pointer(&d)) == 0 {
			return float64(d)
		}
		return 0
	}
	return get("playback-time"), get("duration")
}

// Frame drains events and, if a new frame is due, renders it at w×h into an
// internal BGRA buffer. Returns (buffer, true) when fresh pixels are ready.
func (p *Player) Frame(w, h int) ([]byte, bool) {
	for {
		ev := C.mpv_wait_event(p.h, 0)
		if ev == nil || ev.event_id == C.MPV_EVENT_NONE {
			break
		}
	}
	if C.mpv_render_context_update(p.rc)&C.MPV_RENDER_UPDATE_FRAME == 0 {
		return nil, false
	}
	if p.w != w || p.hgt != h || p.buf == nil {
		p.w, p.hgt = w, h
		p.buf = make([]byte, w*h*4)
	}
	if C.render_sw(p.rc, C.int(w), C.int(h), unsafe.Pointer(&p.buf[0])) < 0 {
		return nil, false
	}
	return p.buf, true
}

func (p *Player) Close() {
	if p.rc != nil {
		C.mpv_render_context_free(p.rc)
	}
	if p.h != nil {
		C.mpv_destroy(p.h)
	}
}
