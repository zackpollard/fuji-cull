// Package mpvgl embeds mpv through its OpenGL render API with zero-copy
// VAAPI interop: decoded frames stay on the GPU and render straight into an
// SDL texture. The software path (internal/mpvsw) costs ~1 core of copy-back
// plus ~1.5 cores of format conversion for the X-H2S's 4K60 10-bit HEVC —
// this path measured ~0.3 cores total for the same clip.
//
// Threading/context contract: the caller owns a dedicated GL context SHARED
// with SDL's renderer context (textures are shared; FBOs are not). New() and
// Render() must run with that mpv context current on the render thread;
// MakeCurrent/CurrentContext are provided for the juggling.
package mpvgl

/*
#cgo pkg-config: mpv sdl2
#include <stdlib.h>
#include <mpv/client.h>
#include <mpv/render_gl.h>
#include <SDL2/SDL.h>
#include <SDL2/SDL_syswm.h>

static void *get_proc_address_mpv(void *ctx, const char *name) {
	return SDL_GL_GetProcAddress(name);
}

// Native display handles: libmpv needs these at render-context creation for
// zero-copy VAAPI interop — without them hwdec silently degrades to
// vaapi-copy (a full core of GPU-to-RAM copy-back at 4K60).
static void *native_wl_display(void *win) {
	SDL_SysWMinfo info;
	SDL_VERSION(&info.version);
	if (!SDL_GetWindowWMInfo((SDL_Window*)win, &info))
		return NULL;
#ifdef SDL_VIDEO_DRIVER_WAYLAND
	if (info.subsystem == SDL_SYSWM_WAYLAND)
		return info.info.wl.display;
#endif
	return NULL;
}
static void *native_x11_display(void *win) {
	SDL_SysWMinfo info;
	SDL_VERSION(&info.version);
	if (!SDL_GetWindowWMInfo((SDL_Window*)win, &info))
		return NULL;
#ifdef SDL_VIDEO_DRIVER_X11
	if (info.subsystem == SDL_SYSWM_X11)
		return info.info.x11.display;
#endif
	return NULL;
}

// Minimal GL loader — just the FBO plumbing (core in GL 3.0+ and GLES2).
typedef void (*fp_genfb)(int, unsigned int*);
typedef void (*fp_bindfb)(unsigned int, unsigned int);
typedef void (*fp_fbtex2d)(unsigned int, unsigned int, unsigned int, unsigned int, int);
typedef unsigned int (*fp_checkfb)(unsigned int);
typedef void (*fp_delfb)(int, const unsigned int*);
typedef void (*fp_getintv)(unsigned int, int*);
typedef void (*fp_flush)(void);

static fp_genfb   p_glGenFramebuffers;
static fp_bindfb  p_glBindFramebuffer;
static fp_fbtex2d p_glFramebufferTexture2D;
static fp_checkfb p_glCheckFramebufferStatus;
static fp_delfb   p_glDeleteFramebuffers;
static fp_getintv p_glGetIntegerv;
static fp_flush   p_glFlush;

#define GL_FRAMEBUFFER_X        0x8D40
#define GL_COLOR_ATTACHMENT0_X  0x8CE0
#define GL_TEXTURE_2D_X         0x0DE1
#define GL_TEXTURE_BINDING_2D_X 0x8069
#define GL_FRAMEBUFFER_COMPLETE_X 0x8CD5

static int load_gl(void) {
	p_glGenFramebuffers        = (fp_genfb)SDL_GL_GetProcAddress("glGenFramebuffers");
	p_glBindFramebuffer        = (fp_bindfb)SDL_GL_GetProcAddress("glBindFramebuffer");
	p_glFramebufferTexture2D   = (fp_fbtex2d)SDL_GL_GetProcAddress("glFramebufferTexture2D");
	p_glCheckFramebufferStatus = (fp_checkfb)SDL_GL_GetProcAddress("glCheckFramebufferStatus");
	p_glDeleteFramebuffers     = (fp_delfb)SDL_GL_GetProcAddress("glDeleteFramebuffers");
	p_glGetIntegerv            = (fp_getintv)SDL_GL_GetProcAddress("glGetIntegerv");
	p_glFlush                  = (fp_flush)SDL_GL_GetProcAddress("glFlush");
	return p_glGenFramebuffers && p_glBindFramebuffer && p_glFramebufferTexture2D &&
	       p_glCheckFramebufferStatus && p_glDeleteFramebuffers && p_glGetIntegerv && p_glFlush;
}

static mpv_render_context *create_gl(mpv_handle *h, void *win) {
	mpv_opengl_init_params gl = { .get_proc_address = get_proc_address_mpv };
	void *wl = native_wl_display(win);
	void *x11 = native_x11_display(win);
	mpv_render_param params[5];
	int n = 0;
	params[n++] = (mpv_render_param){MPV_RENDER_PARAM_API_TYPE, MPV_RENDER_API_TYPE_OPENGL};
	params[n++] = (mpv_render_param){MPV_RENDER_PARAM_OPENGL_INIT_PARAMS, &gl};
	if (wl)
		params[n++] = (mpv_render_param){MPV_RENDER_PARAM_WL_DISPLAY, wl};
	else if (x11)
		params[n++] = (mpv_render_param){MPV_RENDER_PARAM_X11_DISPLAY, x11};
	params[n] = (mpv_render_param){0, NULL};
	mpv_render_context *rc = NULL;
	if (mpv_render_context_create(&rc, h, params) < 0)
		return NULL;
	return rc;
}

static unsigned int make_fbo(unsigned int tex) {
	unsigned int fbo = 0;
	p_glGenFramebuffers(1, &fbo);
	p_glBindFramebuffer(GL_FRAMEBUFFER_X, fbo);
	p_glFramebufferTexture2D(GL_FRAMEBUFFER_X, GL_COLOR_ATTACHMENT0_X, GL_TEXTURE_2D_X, tex, 0);
	unsigned int st = p_glCheckFramebufferStatus(GL_FRAMEBUFFER_X);
	p_glBindFramebuffer(GL_FRAMEBUFFER_X, 0);
	if (st != GL_FRAMEBUFFER_COMPLETE_X) {
		p_glDeleteFramebuffers(1, &fbo);
		return 0;
	}
	return fbo;
}

static void del_fbo(unsigned int fbo) { p_glDeleteFramebuffers(1, &fbo); }

static int render_gl_fbo(mpv_render_context *rc, unsigned int fbo, int w, int h) {
	mpv_opengl_fbo f = { .fbo = (int)fbo, .w = w, .h = h, .internal_format = 0 };
	// No flip: FBO texel rows already match SDL's top-down texture sampling
	// (flip=1 rendered upside down in practice).
	int flip = 0;
	mpv_render_param params[] = {
		{MPV_RENDER_PARAM_OPENGL_FBO, &f},
		{MPV_RENDER_PARAM_FLIP_Y, &flip},
		{0, NULL}
	};
	return mpv_render_context_render(rc, params);
}

static int bound_texture_2d(void) {
	int t = 0;
	p_glGetIntegerv(GL_TEXTURE_BINDING_2D_X, &t);
	return t;
}

static void gl_flush(void) { p_glFlush(); }
static void *current_ctx(void) { return SDL_GL_GetCurrentContext(); }
static int make_current(void *win, void *ctx) { return SDL_GL_MakeCurrent((SDL_Window*)win, (SDL_GLContext)ctx); }

static void cmd1(mpv_handle *h, const char *c0) {
	const char *cmd[] = {c0, NULL};
	mpv_command(h, cmd);
}
static void cmd2(mpv_handle *h, const char *c0, const char *c1) {
	const char *cmd[] = {c0, c1, NULL};
	mpv_command(h, cmd);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// CurrentContext returns the GL context current on this thread (SDL's
// renderer context, when captured right after renderer creation).
func CurrentContext() uintptr { return uintptr(C.current_ctx()) }

// MakeCurrent activates a GL context captured via CurrentContext.
func MakeCurrent(win unsafe.Pointer, ctx uintptr) {
	C.make_current(win, unsafe.Pointer(ctx))
}

// Flush issues glFlush — required after rendering so the shared texture's
// contents are visible from SDL's context.
func Flush() { C.gl_flush() }

// BoundTexture2D reports the GL name of the texture bound to TEXTURE_2D
// (used with SDL's GLBind to learn an SDL texture's GL name).
func BoundTexture2D() uint32 { return uint32(C.bound_texture_2d()) }

// Player is an embedded mpv instance rendering via OpenGL.
type Player struct {
	h  *C.mpv_handle
	rc *C.mpv_render_context

	fbo    C.uint
	fboTex uint32
	fboW   int
	fboH   int
}

// New creates the player and its GL render context. The caller's dedicated
// mpv GL context must be current; win is the SDL window (native display
// handles are extracted from it for zero-copy hwdec interop).
func New(win unsafe.Pointer) (*Player, error) {
	if C.load_gl() == 0 {
		return nil, fmt.Errorf("required GL functions unavailable")
	}
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
	// Native hwdec: with GL interop, decoded VAAPI surfaces display without
	// ever touching system memory (the whole point of this package).
	set("hwdec", "auto")
	set("keep-open", "yes")
	set("audio-client-name", "fuji-cull")
	// Camera streams arrive over loopback HTTP backed by MTP partial reads:
	// buffer generously to ride out chunk-boundary latency.
	set("cache", "yes")
	set("demuxer-max-bytes", "256MiB")
	set("demuxer-readahead-secs", "10")
	if C.mpv_initialize(h) < 0 {
		C.mpv_destroy(h)
		return nil, fmt.Errorf("mpv_initialize failed")
	}
	rc := C.create_gl(h, win)
	if rc == nil {
		C.mpv_destroy(h)
		return nil, fmt.Errorf("mpv render context (opengl) failed")
	}
	return &Player{h: h, rc: rc}, nil
}

// Render draws the current video frame into the SDL texture identified by
// its GL name (mpv GL context must be current). Renders only when mpv has a
// new frame or the target changed; the texture retains the last frame
// otherwise. Returns false when nothing was drawn.
func (p *Player) Render(texID uint32, w, h int) bool {
	for {
		ev := C.mpv_wait_event(p.h, 0)
		if ev == nil || ev.event_id == C.MPV_EVENT_NONE {
			break
		}
	}
	retarget := p.fbo == 0 || p.fboTex != texID || p.fboW != w || p.fboH != h
	if retarget {
		if p.fbo != 0 {
			C.del_fbo(p.fbo)
			p.fbo = 0
		}
		p.fbo = C.make_fbo(C.uint(texID))
		if p.fbo == 0 {
			return false
		}
		p.fboTex, p.fboW, p.fboH = texID, w, h
	}
	if !retarget && C.mpv_render_context_update(p.rc)&C.MPV_RENDER_UPDATE_FRAME == 0 {
		return false
	}
	return C.render_gl_fbo(p.rc, p.fbo, C.int(w), C.int(h)) >= 0
}

// PropertyString returns an mpv property as a string ("" when unavailable).
func (p *Player) PropertyString(name string) string {
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	cs := C.mpv_get_property_string(p.h, cn)
	if cs == nil {
		return ""
	}
	defer C.mpv_free(unsafe.Pointer(cs))
	return C.GoString(cs)
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

// Close frees the render context and player (mpv GL context must be current
// for the render-context teardown).
func (p *Player) Close() {
	if p.fbo != 0 {
		C.del_fbo(p.fbo)
	}
	if p.rc != nil {
		C.mpv_render_context_free(p.rc)
	}
	if p.h != nil {
		C.mpv_destroy(p.h)
	}
}
