package main

import (
	"fmt"

	"github.com/veandco/go-sdl2/sdl"

	"github.com/zack/fuji-tools/internal/cull"
)

// Import panel: keeper summary, destination/album text fields, progress.
func (u *ui) drawImportPanel() {
	w, h := u.outSize()
	// Tall enough for the fields, both toggles AND the progress bar that
	// appears once an import is running — the layout must not collapse at the
	// exact moment you are watching it.
	pw, ph := sc(640), sc(470)
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
	pending, skipped := u.app.PendingImportCounts(u.impReimport)
	u.text(u.fontSm, fmt.Sprintf("shots marked keep: %d    files: %d    size: %.2f GB",
		nShots, nFiles, float64(size)/1e9), colFG, box.X+sc(24), y, false)
	y += sc(20)
	if skipped > 0 {
		u.text(u.fontSm, fmt.Sprintf("this run: %d shots  (%d already imported — skipped)", pending, skipped),
			colAmber, box.X+sc(24), y, false)
	} else {
		u.text(u.fontSm, fmt.Sprintf("this run: %d shots", pending), colDim, box.X+sc(24), y, false)
	}
	y += sc(24)

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
		u.text(u.fontSm, shown, colFG, r.X+sc(8), r.Y+sc(5), false)
		y += sc(38)
	}
	field("destination directory (Tab switches fields)", u.impDest, u.impField == 0)
	field("immich album (optional)", u.impAlbum, u.impField == 1)

	// Two switches, because "copy to disk" and "upload to Immich" are
	// independent jobs and people want each on its own.
	toggle := func(label string, on, active, disabled bool, note string) {
		mark := "[ ] " + label
		if on {
			mark = "[x] " + label
		}
		c := colFG
		if disabled {
			c = colDim
		} else if active {
			c = colAmber
		}
		u.text(u.fontSm, mark, c, box.X+sc(24), y, false)
		if note != "" {
			u.text(u.fontSm, note, colDim, box.X+sc(300), y, false)
		}
		y += sc(22)
	}
	toggle("upload to Immich", u.impImmich, u.impField == 2, !u.immichReady,
		map[bool]string{true: "", false: "no server configured (⌘,)"}[u.immichReady])
	toggle("keep local copy", u.impKeep, u.impField == 3, false,
		map[bool]string{true: "", false: "staged, then deleted once verified"}[u.impKeep])
	toggle("re-import already imported", u.impReimport, u.impField == 4, false,
		map[bool]string{true: "sends finished events again", false: ""}[u.impReimport])
	y += sc(6)

	st := u.app.ImportState()
	if st.Running || st.Phase == "done" || st.Phase == "error" {
		label := st.Phase
		if !st.Running {
			label += " — finished"
		}
		// Copy and upload run at the same time, so each gets its own counter
		// and its own bar — one number standing for both was unreadable.
		uploading := u.impImmich && u.immichReady
		bar := func(label string, done, total int, col sdl.Color) {
			pct := 0
			if total > 0 {
				pct = done * 100 / total
			}
			u.text(u.fontSm, fmt.Sprintf("%-22s %d / %d  (%d%%)", label, done, total, pct),
				colFG, box.X+sc(24), y, false)
			y += sc(20)
			track := sdl.Rect{X: box.X + sc(24), Y: y, W: pw - sc(48), H: sc(8)}
			u.fillRect(track, colBG)
			if total > 0 {
				fill := track
				fill.W = int32(float64(track.W) * float64(done) / float64(total))
				u.fillRect(fill, col)
			}
			y += sc(18)
		}
		status := st.Phase
		if !st.Running {
			status += " — finished"
		}
		u.text(u.fontSm, status, colAmber, box.X+sc(24), y, false)
		y += sc(22)
		bar("download from camera", st.Done, st.Total, colKeep)
		if uploading {
			bar("upload to Immich", st.Uploaded, st.Total, colImmich)
		}
		y += sc(20)
		if st.Error != "" {
			u.text(u.fontSm, st.Error, colReject, box.X+sc(24), y, false)
			y += sc(20)
		}
	}
	u.text(u.fontSm, "Enter start import    Space toggle    Esc close", colDim, box.X+sc(24), box.Y+ph-sc(30), false)
}

func (u *ui) importKey(e *sdl.KeyboardEvent) {
	switch e.Keysym.Sym {
	case sdl.K_ESCAPE:
		u.mode = modeViewer
		sdl.StopTextInput()
	case sdl.K_TAB:
		step := 1
		if e.Keysym.Mod&sdl.KMOD_SHIFT != 0 {
			step = 4
		}
		u.impField = (u.impField + step) % 5
	case sdl.K_SPACE:
		switch u.impField {
		case 2:
			if u.immichReady {
				u.impImmich = !u.impImmich
			}
		case 3:
			u.impKeep = !u.impKeep
		case 4:
			u.impReimport = !u.impReimport
		}
	case sdl.K_RETURN:
		if err := u.app.StartImport(u.impDest, u.impAlbum,
			cull.ImportOptions{Immich: u.impImmich && u.immichReady, KeepLocal: u.impKeep,
				Reimport: u.impReimport}); err != nil {
			u.impError = err.Error()
		} else {
			u.impError = ""
		}
	case sdl.K_v:
		// Cmd+V / Ctrl+V — destination paths get pasted too
		if e.Keysym.Mod&(sdl.KMOD_GUI|sdl.KMOD_CTRL) != 0 {
			switch u.impField {
			case 0:
				u.impDest += clipboardText()
			case 1:
				u.impAlbum += clipboardText()
			}
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
	if sdl.GetModState()&(sdl.KMOD_GUI|sdl.KMOD_CTRL) != 0 {
		return // the "v" of Cmd+V
	}
	switch u.impField {
	case 0:
		u.impDest += t
	case 1:
		u.impAlbum += t
	}
}
