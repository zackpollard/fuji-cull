package main

import (
	"fmt"

	"github.com/veandco/go-sdl2/sdl"
)

// Import panel: keeper summary, destination/album text fields, progress.
func (u *ui) drawImportPanel() {
	w, h := u.outSize()
	pw, ph := sc(640), sc(330)
	box := sdl.Rect{X: (w - pw) / 2, Y: (h - ph) / 2, W: pw, H: ph}
	u.fillRect(sdl.Rect{X: 0, Y: 0, W: w, H: h}, sdl.Color{R: 0, G: 0, B: 0, A: 180})
	u.fillRect(box, colPanel)
	u.ren.SetDrawColor(colDim.R, colDim.G, colDim.B, 255)
	u.ren.DrawRect(&box)

	nShots, nFiles, size := 0, 0, int64(0)
	for _, s := range u.shots {
		if u.decisions[s.ID] != "keep" {
			continue
		}
		nShots++
		nFiles += len(s.Files)
		size += s.TotalSize()
	}
	y := box.Y + sc(20)
	u.text(u.font, "IMPORT KEEPERS", colAmber, box.X+sc(24), y, false)
	y += sc(34)
	u.text(u.fontSm, fmt.Sprintf("shots marked keep: %d    files: %d    size: %.2f GB",
		nShots, nFiles, float64(size)/1e9), colFG, box.X+sc(24), y, false)
	y += sc(34)

	field := func(label, val string, active bool) {
		u.text(u.fontSm, label, colDim, box.X+sc(24), y, false)
		y += sc(20)
		r := sdl.Rect{X: box.X + sc(24), Y: y, W: pw - sc(48), H: sc(26)}
		u.fillRect(r, colBG)
		c := colDim
		if active {
			c = colAmber
		}
		u.ren.SetDrawColor(c.R, c.G, c.B, 255)
		u.ren.DrawRect(&r)
		shown := val
		if active {
			shown += "_"
		}
		u.text(u.fontSm, shown, colFG, r.X+8, r.Y+5, false)
		y += 38
	}
	field("destination directory (Tab switches fields)", u.impDest, u.impField == 0)
	field("immich album (optional)", u.impAlbum, u.impField == 1)

	st := u.app.ImportState()
	if st.Running || st.Phase == "done" || st.Phase == "error" {
		label := st.Phase
		if !st.Running {
			label += " — finished"
		}
		u.text(u.fontSm, fmt.Sprintf("%s   %d / %d", label, st.Done, st.Total), colAmber, box.X+sc(24), y, false)
		y += 22
		bar := sdl.Rect{X: box.X + sc(24), Y: y, W: pw - sc(48), H: 8}
		u.fillRect(bar, colBG)
		if st.Total > 0 {
			fill := bar
			fill.W = int32(float64(bar.W) * float64(st.Done) / float64(st.Total))
			u.fillRect(fill, colKeep)
		}
		y += sc(20)
		if st.Error != "" {
			u.text(u.fontSm, st.Error, colReject, box.X+sc(24), y, false)
			y += sc(20)
		}
	}
	u.text(u.fontSm, "Enter start import    Esc close", colDim, box.X+sc(24), box.Y+ph-30, false)
}

func (u *ui) importKey(e *sdl.KeyboardEvent) {
	switch e.Keysym.Sym {
	case sdl.K_ESCAPE:
		u.mode = modeViewer
		sdl.StopTextInput()
	case sdl.K_TAB:
		u.impField = 1 - u.impField
	case sdl.K_RETURN:
		if err := u.app.StartImport(u.impDest, u.impAlbum); err != nil {
			u.impError = err.Error()
		} else {
			u.impError = ""
		}
	case sdl.K_BACKSPACE:
		f := &u.impDest
		if u.impField == 1 {
			f = &u.impAlbum
		}
		if len(*f) > 0 {
			*f = (*f)[:len(*f)-1]
		}
	}
}

func (u *ui) importText(t string) {
	if u.impField == 0 {
		u.impDest += t
	} else {
		u.impAlbum += t
	}
}
