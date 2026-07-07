package main

import (
	"unsafe"

	"github.com/veandco/go-sdl2/sdl"

	"github.com/zack/fuji-tools/internal/mpvsw"
)

// drawVideo renders the current video shot: embedded mpv playback when the
// file is buffered locally, otherwise the pull-on-demand placeholder.
func (u *ui) drawVideo(st sdl.Rect) {
	s := u.shots[u.cursor]
	path, ready := u.app.VideoPathIfReady(s.ID)

	if !ready {
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
		p, err := mpvsw.New()
		if err != nil {
			u.text(u.font, "mpv unavailable: "+err.Error(), colReject, st.X+st.W/2, st.Y+st.H/2, true)
			return
		}
		u.mpv = p
	}
	if u.videoID != s.ID {
		u.videoID = s.ID
		u.mpv.Load(path)
	}

	// keep the streaming texture matched to the stage size
	if u.videoTex == nil || u.videoTexW != st.W || u.videoTexH != st.H {
		if u.videoTex != nil {
			u.videoTex.Destroy()
		}
		t, err := u.ren.CreateTexture(uint32(sdl.PIXELFORMAT_ARGB8888), sdl.TEXTUREACCESS_STREAMING, st.W, st.H)
		if err != nil {
			return
		}
		u.videoTex, u.videoTexW, u.videoTexH = t, st.W, st.H
	}
	if buf, ok := u.mpv.Frame(int(st.W), int(st.H)); ok {
		u.videoTex.Update(nil, unsafe.Pointer(&buf[0]), int(st.W)*4)
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
		u.videoID = ""
	}
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
