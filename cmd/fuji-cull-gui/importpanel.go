package main

import (
	"fmt"

	"github.com/veandco/go-sdl2/sdl"
	"github.com/veandco/go-sdl2/ttf"

	"github.com/zack/fuji-tools/internal/cull"
)

// colPending is the muted token pending lanes are drawn in — a lane that has
// not started must not read as one that is stalled.
var colPending = sdl.Color{R: 0x6B, G: 0x6E, B: 0x62, A: 255}

// textW measures rendered width; 0 when it cannot be known.
func (u *ui) textW(f *ttf.Font, s string) int32 {
	if s == "" || f == nil {
		return 0
	}
	w, _, err := f.SizeUTF8(s)
	if err != nil {
		return 0
	}
	return int32(w)
}

// rightText draws s with its RIGHT edge at `right`, so lane counters line up
// down the panel however wide the numbers get.
func (u *ui) rightText(f *ttf.Font, s string, c sdl.Color, right, y int32) {
	if s == "" || f == nil {
		return
	}
	u.text(f, s, c, right-u.textW(f, s), y, false)
}

// Import panel: keeper summary, destination/album text fields, progress.
func (u *ui) drawImportPanel() {
	w, h := u.outSize()
	// Tall enough for the fields, both toggles AND the progress bar that
	// appears once an import is running — the layout must not collapse at the
	// exact moment you are watching it.
	pw, ph := sc(640), sc(510)
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
		// One lane per stage. Copy, hash and upload overlap by design, so
		// every lane is drawn every frame and any number of them may be
		// moving — none of them is "the current step".
		//
		// The panel is 640 px wide, so lanes take the inline form the design
		// specifies above 640: label, bar, right-aligned counter.
		//
		// The bar ends where the counter starts, MEASURED — a fixed counter
		// column is only ever right for one counter, and the camera lane's
		// (files, bytes, cached, rate) overran it and drew the number on top of
		// its own bar.
		const labelW = 62
		barX := box.X + sc(24+labelW)
		numRight := box.X + pw - sc(24)
		barWidthFor := func(counter string) int32 {
			w := numRight - u.textW(u.fontSm, counter) - sc(14) - barX
			if min := sc(60); w < min {
				w = min // a squeezed bar still has to read as a bar
			}
			return w
		}

		lane := func(name string, col sdl.Color, num, den float64, counter string, pending bool) {
			nameCol, numCol := col, colFG
			if pending {
				nameCol, numCol = colPending, colPending
			}
			u.text(u.fontSm, name, nameCol, box.X+sc(24), y, false)
			barW := barWidthFor(counter)
			track := sdl.Rect{X: barX, Y: y + sc(5), W: barW, H: sc(6)}
			u.fillRect(track, colBG)
			if den > 0 && num > 0 {
				fill := track
				fill.W = int32(float64(barW) * minf(num/den, 1))
				u.fillRect(fill, col)
			}
			u.rightText(u.fontSm, counter, numCol, numRight, y)
			y += sc(22)
		}
		// Text-only lane: verify is one bulk checksum query lasting a second
		// or two, so a bar for it would be a bar nobody can watch.
		textLane := func(name string, col sdl.Color, counter string, pending bool) {
			nameCol, numCol := col, colFG
			if pending {
				nameCol, numCol = colPending, colPending
			}
			u.text(u.fontSm, name, nameCol, box.X+sc(24), y, false)
			u.rightText(u.fontSm, counter, numCol, numRight, y)
			y += sc(22)
		}

		header := "IMPORT"
		if st.ElapsedSec > 0 {
			header += fmt.Sprintf("   %d:%02d", st.ElapsedSec/60, st.ElapsedSec%60)
		}
		if !st.Running {
			header += " — finished"
		}
		u.text(u.fontSm, header, colAmber, box.X+sc(24), y, false)
		y += sc(24)

		// Bars are byte-denominated where bytes are known: a file count cannot
		// tell a 25 MB JPEG from a 62 MB RAF, and that gap is most of an import.
		cam := st.Camera
		camCounter := fmt.Sprintf("%d / %d · %s", cam.Files, cam.FilesTotal, humanBytes(cam.Bytes))
		if cam.Cached > 0 {
			camCounter += fmt.Sprintf(" · %d cached", cam.Cached)
		}
		if cam.Rate > 0 {
			camCounter += " · " + humanRate(cam.Rate)
		}
		lane("CAMERA", colKeep, float64(cam.Bytes), float64(cam.BytesTotal), camCounter,
			cam.State == "pending")

		if up := st.Upload; up.State != "n/a" {
			counter := fmt.Sprintf("%d / %d · %s", up.Files, up.FilesTotal, humanBytes(up.Bytes))
			if up.Rate > 0 {
				counter += " · " + humanRate(up.Rate)
			}
			if up.Failed > 0 {
				counter += fmt.Sprintf(" · %d failed", up.Failed)
			}
			lane("UPLOAD", colImmich, float64(up.Bytes), float64(up.BytesTotal), counter,
				up.State == "pending")
		}
		if sk := st.Stack; sk.State != "n/a" {
			counter := fmt.Sprintf("%d / %d pairs", sk.Files, sk.FilesTotal)
			if sk.Rate > 0 {
				counter += fmt.Sprintf(" · %.1f pairs/s", sk.Rate)
			}
			if sk.Failed > 0 {
				counter += fmt.Sprintf(" · %d failed", sk.Failed)
			}
			lane("STACK", colBuffered, float64(sk.Files), float64(sk.FilesTotal), counter,
				sk.State == "pending")
		}
		if v := st.Verify; v.State != "n/a" {
			counter := "after the last upload"
			if v.State != "pending" {
				counter = fmt.Sprintf("%d / %d on server", v.Files, v.FilesTotal)
			}
			textLane("VERIFY", colDim, counter, v.State == "pending")
		}

		// Per-file bytes: a multi-GB video sits on one file for minutes, and
		// no lane counter moves at all while it does.
		if st.FileTotal > 0 {
			y += sc(6)
			counter := humanBytes(st.FileSent) + " / " + humanBytes(st.FileTotal)
			if st.RateBps > 0 {
				counter += " · " + humanRate(st.RateBps)
			}
			u.text(u.fontSm, trimName(st.File), colFG, box.X+sc(24), y, false)
			fbw := barWidthFor(counter)
			track := sdl.Rect{X: barX, Y: y + sc(6), W: fbw, H: sc(3)}
			u.fillRect(track, colBG)
			fill := track
			fill.W = int32(float64(fbw) * minf(float64(st.FileSent)/float64(st.FileTotal), 1))
			u.fillRect(fill, colAmber)
			u.rightText(u.fontSm, counter, colDim, numRight, y)
			y += sc(22)
		}
		y += sc(14)
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

// humanBytes formats a byte count for a progress line.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/1024)
	}
}

// humanRate formats an upload rate, in whichever unit keeps it readable.
func humanRate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KB/s", bps/(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

// trimName keeps the panel from wrapping on a long filename.
func trimName(n string) string {
	if len(n) > 20 {
		return n[:19] + "…"
	}
	return n
}
