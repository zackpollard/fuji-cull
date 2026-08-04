package main

import (
	"fmt"
	"log"
	"math"
	"strings"

	"github.com/veandco/go-sdl2/sdl"
	"github.com/veandco/go-sdl2/ttf"
)

// Settings panel. Desktop previously had no way to enter Immich credentials —
// flags and environment only — and an app launched from Finder inherits
// neither, so double-clicking the icon could never reach a server. This is that
// missing screen: comma opens it, Tab moves between fields, Enter saves.
//
// Saving applies immediately (the import pipeline reads its options per run)
// and persists, so the credentials survive a relaunch from the Dock.

const (
	setFieldURL = iota
	setFieldKey
	setFieldAlbum
	setFieldStack
	setFieldCount
)

// openSettings loads the live values into the editable copies.
func (u *ui) openSettings() {
	url, key, album, stack, _ := u.app.ImmichSettings()
	u.setURL, u.setKey, u.setAlbum, u.setStack = url, key, album, stack
	u.setField = setFieldURL
	u.setSaved = false
	u.setClearArmed = false
	u.setPrevMode = u.mode
	u.mode = modeSettings
	sdl.StartTextInput()
}

// maskKey shows enough of a stored key to recognise it without printing a
// credential in full on a screen someone might be sharing.
func maskKey(k string, active bool) string {
	if active { // editing: show what you're typing
		return k
	}
	if k == "" {
		return ""
	}
	if len(k) <= 6 {
		return strings.Repeat("•", len(k))
	}
	return strings.Repeat("•", len(k)-4) + k[len(k)-4:]
}

func (u *ui) drawSettings() {
	w, h := u.outSize()
	pw := sc(640)
	// Wrap the help against the real font metrics rather than guessing at a
	// character count: the first version ran straight out of the panel.
	helpLines := u.wrapText(u.fontSm, permHelp, pw-sc(48))
	ph := sc(348) + int32(len(helpLines))*sc(16)
	box := sdl.Rect{X: (w - pw) / 2, Y: (h - ph) / 2, W: pw, H: ph}
	u.fillRect(sdl.Rect{X: 0, Y: 0, W: w, H: h}, sdl.Color{R: 0, G: 0, B: 0, A: 180})
	u.fillRect(box, colPanel)
	u.ren.SetDrawColor(colDim.R, colDim.G, colDim.B, 255)
	u.ren.DrawRect(&box)

	y := box.Y + sc(20)
	u.text(u.font, "SETTINGS", colAmber, box.X+sc(24), y, false)
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
		u.text(u.fontSm, shown, colFG, r.X+sc(8), r.Y+sc(5), false)
		y += sc(38)
	}

	field("immich url (Tab switches fields)", u.setURL, u.setField == setFieldURL)
	field("immich api key", maskKey(u.setKey, u.setField == setFieldKey), u.setField == setFieldKey)
	field("immich album (optional)", u.setAlbum, u.setField == setFieldAlbum)

	// stack toggle, drawn as a field so Tab reaches it the same way
	u.text(u.fontSm, "stack RAF+JPG pairs after upload (Space toggles)", colDim, box.X+sc(24), y, false)
	y += sc(20)
	mark := "[ ] off"
	if u.setStack {
		mark = "[x] on"
	}
	mc := colFG
	if u.setField == setFieldStack {
		mc = colAmber
	}
	u.text(u.fontSm, mark, mc, box.X+sc(24), y, false)
	y += sc(30)

	// Forgetting what has been imported puts every keeper back in the queue,
	// so it asks twice rather than acting on one keystroke.
	imported := u.app.ImportedCount()
	label := fmt.Sprintf("shift+X  forget %d imported shot(s) — puts them back in the import queue", imported)
	c := colDim
	if imported == 0 {
		label = "nothing recorded as imported"
	} else if u.setClearArmed {
		label = fmt.Sprintf("press shift+X again to forget %d imported shot(s)", imported)
		c = colReject
	}
	u.text(u.fontSm, label, c, box.X+sc(24), y, false)
	y += sc(26)

	// Honest state line: say whether uploads are actually possible.
	if u.setSaved {
		u.text(u.fontSm, "saved — imports will upload from now on", colKeep, box.X+sc(24), y, false)
	} else if strings.TrimSpace(u.setURL) == "" || strings.TrimSpace(u.setKey) == "" {
		u.text(u.fontSm, "incomplete — imports copy to disk only", colDim, box.X+sc(24), y, false)
	}

	// Nothing else tells you what to tick when creating the key, and an
	// under-scoped key fails at import time rather than here.
	hy := box.Y + ph - sc(18) - int32(len(helpLines))*sc(16)
	for _, ln := range helpLines {
		u.text(u.fontSm, ln, colDim, box.X+sc(24), hy, false)
		hy += sc(16)
	}
	u.text(u.fontSm, "Enter save    Esc close", colDim, box.X+sc(24), box.Y+ph-sc(18), false)
}

func (u *ui) settingsKey(e *sdl.KeyboardEvent) {
	switch e.Keysym.Sym {
	case sdl.K_ESCAPE:
		u.mode = u.setPrevMode
		sdl.StopTextInput()
	case sdl.K_TAB:
		step := 1
		if e.Keysym.Mod&sdl.KMOD_SHIFT != 0 {
			step = setFieldCount - 1
		}
		u.setField = (u.setField + step) % setFieldCount
		u.setSaved = false
	case sdl.K_SPACE:
		if u.setField == setFieldStack {
			u.setStack = !u.setStack
			u.setSaved = false
		}
	case sdl.K_v:
		// Cmd+V / Ctrl+V. An API key is far too long to type, so paste is not
		// a nicety here — without it the field is unusable.
		if e.Keysym.Mod&(sdl.KMOD_GUI|sdl.KMOD_CTRL) != 0 {
			if f := u.setTarget(); f != nil {
				*f += clipboardText()
				u.setSaved = false
			}
		}
	case sdl.K_x:
		// Destructive, so it takes two presses and never a bare key.
		if e.Keysym.Mod&sdl.KMOD_SHIFT == 0 {
			break
		}
		if !u.setClearArmed {
			u.setClearArmed = true
			break
		}
		u.setClearArmed = false
		if n, err := u.app.ClearImported(); err != nil {
			log.Printf("clear imported: %v", err)
		} else {
			log.Printf("import: forgot %d imported marker(s)", n)
		}
	case sdl.K_BACKSPACE:
		if f := u.setTarget(); f != nil && len(*f) > 0 {
			*f = (*f)[:len(*f)-1]
			u.setSaved = false
		}
	case sdl.K_RETURN, sdl.K_KP_ENTER:
		u.app.SetImmich(u.setURL, u.setKey, u.setAlbum, u.setStack)
		// keep the panel open so the confirmation is visible
		u.setSaved = true
		// the import panel prefills from the same album
		u.impAlbum = strings.TrimSpace(u.setAlbum)
	}
}

// setTarget is the string currently being edited, or nil for the toggle.
func (u *ui) setTarget() *string {
	switch u.setField {
	case setFieldURL:
		return &u.setURL
	case setFieldKey:
		return &u.setKey
	case setFieldAlbum:
		return &u.setAlbum
	}
	return nil
}

func (u *ui) settingsText(t string) {
	// The "v" of Cmd+V can still arrive as text input; don't type it.
	if sdl.GetModState()&(sdl.KMOD_GUI|sdl.KMOD_CTRL) != 0 {
		return
	}
	if f := u.setTarget(); f != nil {
		*f += t
		u.setSaved = false
	}
}

// permHelp is what to tick when creating the key in Immich. An under-scoped
// key saves here without complaint and only fails at import time, so it is
// worth stating up front.
const permHelp = "key permissions: asset.upload · albums also need album.read, " +
	"album.create, albumAsset.create · stacking also needs stack.create"

// clipboardText returns the clipboard contents with surrounding whitespace
// removed — copying a key out of a web page routinely picks up a trailing
// newline, which would otherwise be pasted into the credential.
func clipboardText() string {
	t, err := sdl.GetClipboardText()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(t)
}

// wrapText greedily wraps s to lines that fit maxW pixels, measured with the
// font that will draw them.
func (u *ui) wrapText(f *ttf.Font, s string, maxW int32) []string {
	words := strings.Fields(s)
	if len(words) == 0 || f == nil {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		try := cur + " " + w
		if tw, _, err := f.SizeUTF8(try); err == nil && int32(tw) > maxW {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur = try
	}
	return append(lines, cur)
}

/* ── header settings button ───────────────────────────────── */

// ptIn reports whether a point is inside a rect (hit-testing header buttons).
func ptIn(r sdl.Rect, x, y int32) bool {
	return r.W > 0 && x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

// fillCircle draws a filled circle by horizontal spans. SDL's renderer has no
// circle primitive, and the mono UI font has no cog glyph to lean on, so the
// icon is drawn rather than typed.
func (u *ui) fillCircle(cx, cy, r int32, c sdl.Color) {
	u.ren.SetDrawColor(c.R, c.G, c.B, 255)
	for dy := -r; dy <= r; dy++ {
		dx := int32(math.Sqrt(float64(r*r-dy*dy)) + 0.5)
		u.ren.DrawLine(cx-dx, cy+dy, cx+dx, cy+dy)
	}
}

// drawCog renders a gear at (cx, cy) and records its hit rect so a click can
// find it. Lights up on hover and while the panel is open.
func (u *ui) drawCog(cx, cy, r int32) {
	c := colDim
	hot := ptIn(u.cogRect, u.mouseX, u.mouseY) || u.mode == modeSettings
	if hot {
		c = colAmber
	}
	// teeth first, so the body covers their inner ends
	tooth := r / 2
	for i := 0; i < 8; i++ {
		a := float64(i) * math.Pi / 4
		tx := cx + int32(math.Round(math.Cos(a)*float64(r)))
		ty := cy + int32(math.Round(math.Sin(a)*float64(r)))
		u.fillRect(sdl.Rect{X: tx - tooth/2, Y: ty - tooth/2, W: tooth, H: tooth}, c)
	}
	u.fillCircle(cx, cy, r, c)
	u.fillCircle(cx, cy, r/2, colPanel) // hub
	u.cogRect = sdl.Rect{X: cx - r - tooth, Y: cy - r - tooth, W: 2 * (r + tooth), H: 2 * (r + tooth)}
}
