package main

import (
	"log"
	"net/url"
	"unsafe"

	"github.com/veandco/go-sdl2/sdl"

	"github.com/zack/fuji-tools/internal/mpvgl"
	"github.com/zack/fuji-tools/internal/mpvsw"
)

// drawVideo renders the current video shot: embedded mpv playback from the
// local buffer when pulled, else streamed straight off the camera; the
// pull-on-demand placeholder only remains for when neither is possible.
func (u *ui) drawVideo(st sdl.Rect) {
	s := u.shots[u.cursor]
	src, streaming := "", false
	if path, ready := u.app.VideoPathIfReady(s.ID); ready {
		src = path
	} else if u.fetchStates[s.ID] != "fetching" && u.app.CanStreamVideo(s.ID) {
		src = u.apiBase + "/api/video?id=" + url.QueryEscape(s.ID)
		streaming = true
	}

	if src == "" {
		if u.videoID == s.ID {
			u.stopVideo() // stream gave way to an explicit pull
		}
		state := u.fetchStates[s.ID]
		msg, sub := "VIDEO NOT BUFFERED", "press L to pull "+s.Base+" from the camera"
		if state == "fetching" {
			msg, sub = "PULLING VIDEO "+s.Base, "this can take a while for long clips"
		} else if state == "failed" {
			msg, sub = "PULL FAILED", "press L to retry"
		}
		u.text(u.font, msg, colAmber, st.X+st.W/2, st.Y+st.H/2-14, true)
		u.text(u.fontSm, sub, colDim, st.X+st.W/2, st.Y+st.H/2+12, true)
		return
	}

	if u.mpv == nil {
		if u.glVideo {
			u.win.GLMakeCurrent(u.mpvCtx)
			p, err := mpvgl.New()
			mpvgl.MakeCurrent(unsafe.Pointer(u.win), u.sdlCtx)
			if err != nil {
				log.Printf("gui: mpv GL init failed (%v); falling back to software video", err)
				u.glVideo = false
			} else {
				u.mpv = p
			}
		}
		if u.mpv == nil {
			p, err := mpvsw.New()
			if err != nil {
				u.text(u.font, "mpv unavailable: "+err.Error(), colReject, st.X+st.W/2, st.Y+st.H/2, true)
				return
			}
			u.mpv = p
		}
	}
	if u.videoSrc != src {
		u.videoID, u.videoSrc = s.ID, src
		u.videoDiag = false
		u.mpv.Load(src)
	}
	// One diagnostic line per clip once decode settles: proves whether hwdec
	// engaged (silent software fallback of 4K HEVC pegs the CPU).
	if !u.videoDiag {
		if hw := u.mpv.PropertyString("hwdec-current"); hw != "" && hw != "no" || u.mpv.PropertyString("vo-configured") == "yes" {
			log.Printf("video: %s decode=%s format=%s %sx%s@%s drops=%s",
				s.Base, orUnknown(hw),
				u.mpv.PropertyString("video-params/pixelformat"),
				u.mpv.PropertyString("video-params/w"), u.mpv.PropertyString("video-params/h"),
				u.mpv.PropertyString("container-fps"),
				u.mpv.PropertyString("frame-drop-count"))
			u.videoDiag = true
		}
	}
	if streaming {
		u.text(u.fontSm, "STREAMING FROM CAMERA · L pulls a local copy", colDim, st.X+st.W/2, st.Y+16, true)
	}

	// keep the video texture matched to the stage size. GL path: a TARGET
	// texture mpv renders into via FBO; SW path: a STREAMING upload target.
	if u.videoTex == nil || u.videoTexW != st.W || u.videoTexH != st.H {
		if u.videoTex != nil {
			u.videoTex.Destroy()
		}
		access := sdl.TEXTUREACCESS_STREAMING
		if u.glVideo {
			access = sdl.TEXTUREACCESS_TARGET
		}
		t, err := u.ren.CreateTexture(uint32(sdl.PIXELFORMAT_ARGB8888), access, st.W, st.H)
		if err != nil {
			return
		}
		u.videoTex, u.videoTexW, u.videoTexH = t, st.W, st.H
		if u.glVideo {
			// learn the texture's GL name so mpv can render into it
			t.GLBind(nil, nil)
			u.videoTexID = mpvgl.BoundTexture2D()
			t.GLUnbind()
		}
	}
	switch p := u.mpv.(type) {
	case *mpvgl.Player:
		// mpv renders on its own shared context; SDL's GL state is untouched
		// and the texture is shared, so compositing below just works.
		u.win.GLMakeCurrent(u.mpvCtx)
		p.Render(u.videoTexID, int(st.W), int(st.H))
		mpvgl.Flush()
		mpvgl.MakeCurrent(unsafe.Pointer(u.win), u.sdlCtx)
	case *mpvsw.Player:
		if buf, ok := p.Frame(int(st.W), int(st.H)); ok {
			u.videoTex.Update(nil, unsafe.Pointer(&buf[0]), int(st.W)*4)
		}
	}
	u.ren.Copy(u.videoTex, nil, &st)

	// seek bar
	pos, dur := u.mpv.Position()
	if dur > 0 {
		bar := sdl.Rect{X: st.X + 20, Y: st.Y + st.H - 26, W: st.W - 40, H: 6}
		u.fillRect(bar, sdl.Color{R: 30, G: 32, B: 30, A: 220})
		fill := bar
		fill.W = int32(float64(bar.W) * (pos / dur))
		u.fillRect(fill, colAmber)
		u.videoBar = bar
		label := fmtTime(pos) + " / " + fmtTime(dur)
		if u.mpv.Paused() {
			label += "  ⏸"
		}
		u.text(u.fontSm, label, colFG, st.X+st.W/2, st.Y+st.H-48, true)
	}
}

// stopVideo halts playback when navigating away from a video shot.
func (u *ui) stopVideo() {
	if u.mpv != nil && u.videoID != "" {
		u.mpv.Stop()
		u.videoID, u.videoSrc = "", ""
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func fmtTime(s float64) string {
	sec := int(s)
	return pad(sec/60) + ":" + pad(sec%60)
}

func pad(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
