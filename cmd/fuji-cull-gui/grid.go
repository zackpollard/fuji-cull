package main

import (
	"fmt"

	"github.com/veandco/go-sdl2/sdl"
)

// Grid view: full-window thumbnail contact sheet, grouped into an
// Immich-style timeline (month bands + day headers) with a right-edge date
// scrubber. Same culling keys as the viewer; T/Escape return, Enter opens
// the shot. Cell metrics scale with the UI (sc()) and the column count.
//
// cellW/cellH define the BASE aspect ratio (148:100); the actual drawn cell
// size comes from u.cellSize(), which fills the width for the chosen columns.
func cellW() int32   { return sc(148) }
func cellH() int32   { return sc(100) }
func cellGap() int32 { return sc(6) }
func gridPad() int32 { return sc(12) }

const gridTopBar = 44 // sc() applied at use

func (u *ui) gridViewH() int32 {
	_, h := u.outSize()
	return h - sc(gridTopBar)
}

func (u *ui) drawGrid() {
	w, h := u.outSize()
	u.ensureTimeline()
	tl := &u.tl
	top := sc(gridTopBar)
	viewH := h - top

	// Keep the cursor visible only when it MOVES — snapping every frame
	// fights manual scrolling.
	if u.cursor != u.lastGridCursor {
		u.lastGridCursor = u.cursor
		if u.cursor >= 0 && u.cursor < len(tl.shotRow) {
			row := tl.rows[tl.shotRow[u.cursor]]
			// bring the row (and the day header just above, when present)
			// into view
			topWant := row.y
			if tl.shotRow[u.cursor] > 0 && tl.rows[tl.shotRow[u.cursor]-1].kind != tlCells {
				topWant = tl.rows[tl.shotRow[u.cursor]-1].y
			}
			if topWant < u.gridScrollY {
				u.gridScrollY = topWant
			}
			if row.y+row.h > u.gridScrollY+viewH {
				u.gridScrollY = row.y + row.h - viewH
			}
		}
	}
	maxScroll := tl.contentH - viewH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if u.gridScrollY < 0 {
		u.gridScrollY = 0
	}
	if u.gridScrollY > maxScroll {
		u.gridScrollY = maxScroll
	}

	cw, ch := tl.cw, tl.ch
	first := tl.firstVisibleRow(u.gridScrollY)

	// retarget the thumbnail sweep at the first visible shot
	if first < len(tl.rows) {
		hint := 0
		for r := first; r < len(tl.rows); r++ {
			if tl.rows[r].kind == tlCells {
				hint = tl.rows[r].start
				break
			}
		}
		if hint != u.lastHint {
			u.lastHint = hint
			go u.app.SetThumbHint(hint)
		}
	}

	decodes := 0
	for r := first; r < len(tl.rows); r++ {
		row := tl.rows[r]
		sy := top + row.y - u.gridScrollY
		if sy >= h {
			break
		}
		switch row.kind {
		case tlMonth:
			u.text(u.font, row.label, colFG, gridPad(), sy+sc(18), false)
		case tlDay:
			u.text(u.fontSm, row.label, colDim, gridPad(), sy+sc(6), false)
		case tlCells:
			for c := 0; c < row.n; c++ {
				idx := row.start + c
				s := u.shots[idx]
				x := gridPad() + int32(c)*(cw+cellGap())
				cell := sdl.Rect{X: x, Y: sy, W: cw, H: ch}
				u.fillRect(cell, colTickBG)
				if tp, ok := u.app.ThumbPathIfReady(s.ID); ok {
					te := u.thumbs.get(s.ID)
					if (te == nil || te.orient != u.orientAt(idx)) && decodes < 12 {
						te = u.thumbTex(s.ID, tp, u.orientAt(idx))
						decodes++
					}
					if te != nil {
						src := coverSrc(te.w, te.h, cw, ch)
						u.ren.Copy(te.tex, &src, &cell)
					}
				}
				if s.Kind == "video" {
					u.fillRect(sdl.Rect{X: x, Y: sy, W: cw, H: sc(3)}, colAmber)
				}
				if idx < len(u.immich) && u.immich[idx] == '1' {
					u.fillRect(sdl.Rect{X: x + cw - sc(12), Y: sy + sc(6), W: sc(8), H: sc(8)}, colKeep)
				}
				if d := u.decisions[s.ID]; d != "" {
					col := colKeep
					if d == "reject" {
						col = colReject
					}
					u.fillRect(sdl.Rect{X: x, Y: sy + ch - sc(4), W: cw, H: sc(4)}, col)
				}
				if idx == u.cursor {
					u.ren.SetDrawColor(colFG.R, colFG.G, colFG.B, 255)
					u.ren.DrawRect(&sdl.Rect{X: x - sc(2), Y: sy - sc(2), W: cw + sc(4), H: ch + sc(4)})
					u.ren.DrawRect(&sdl.Rect{X: x - sc(1), Y: sy - sc(1), W: cw + sc(2), H: ch + sc(2)})
				}
			}
		}
	}

	// Preload just past the fold with whatever decode budget is left.
	for r := first; r < len(tl.rows) && decodes < 12; r++ {
		row := tl.rows[r]
		if row.kind != tlCells || top+row.y-u.gridScrollY < h {
			continue // already drawn or a header
		}
		for c := 0; c < row.n && decodes < 12; c++ {
			idx := row.start + c
			s := u.shots[idx]
			tp, ok := u.app.ThumbPathIfReady(s.ID)
			if !ok {
				continue
			}
			if te := u.thumbs.get(s.ID); te != nil && te.orient == u.orientAt(idx) {
				continue
			}
			u.thumbTex(s.ID, tp, u.orientAt(idx))
			decodes++
		}
	}

	u.drawScrubber(w, h, top, viewH, maxScroll)

	u.text(u.fontSm, fmt.Sprintf("GRID  %d/%d   T/Esc close   Enter open   [ ] cols   PgUp/Dn day   Ctrl+R rescan   W/K keep   S/X reject",
		u.cursor+1, len(u.shots)), colDim, w/2, h-sc(16), true)
}

// drawScrubber renders the right-edge date rail + draggable handle.
func (u *ui) drawScrubber(w, h, top, viewH, maxScroll int32) {
	tl := &u.tl
	if tl.contentH <= viewH {
		return // everything fits; no scrubber needed
	}
	// month labels down the rail, thinned so they never overlap
	var lastY int32 = -1 << 30
	for _, row := range tl.rows {
		if row.kind != tlMonth {
			continue
		}
		frac := float64(row.y) / float64(tl.contentH)
		ly := top + int32(frac*float64(viewH-sc(16)))
		if ly-lastY < sc(18) {
			continue
		}
		lastY = ly
		col := colDim
		if u.scrubDrag {
			col = colFG
		}
		u.text(u.fontSm, prettyMonthShort(row.key), col, w-sc(58), ly, false)
	}
	// handle
	handleH := sc(40)
	frac := 0.0
	if maxScroll > 0 {
		frac = float64(u.gridScrollY) / float64(maxScroll)
	}
	hy := top + int32(frac*float64(viewH-handleH))
	u.fillRect(sdl.Rect{X: w - sc(10), Y: hy, W: sc(6), H: handleH}, colAmber)
	// month bubble while dragging
	if u.scrubDrag {
		if lbl := u.monthAtScroll(); lbl != "" {
			u.fillRect(sdl.Rect{X: w - sc(150), Y: hy, W: sc(132), H: sc(26)}, colTickBG)
			u.text(u.font, lbl, colFG, w-sc(142), hy+sc(4), false)
		}
	}
}

// monthAtScroll returns the full month label at the current scroll position.
func (u *ui) monthAtScroll() string {
	tl := &u.tl
	viewH := u.gridViewH()
	target := u.gridScrollY + viewH/4
	label := ""
	for _, row := range tl.rows {
		if row.kind == tlMonth {
			if row.y > target {
				break
			}
			label = row.label
		}
	}
	return label
}

// gridClick maps a mouse click to a cell index (-1 when outside a cell, over
// a header, or over the scrubber rail).
func (u *ui) gridClick(mx, my int32) int {
	u.ensureTimeline()
	tl := &u.tl
	top := sc(gridTopBar)
	if my < top {
		return -1
	}
	idx := tl.rowAt(my - top + u.gridScrollY)
	if idx < 0 || tl.rows[idx].kind != tlCells {
		return -1
	}
	row := tl.rows[idx]
	c := int((mx - gridPad()) / (tl.cw + cellGap()))
	if c < 0 || c >= row.n {
		return -1
	}
	return row.start + c
}

// scrubHit reports whether a click x is over the scrubber rail.
func (u *ui) scrubHit(mx int32) bool {
	w, _ := u.outSize()
	return u.tl.contentH > u.gridViewH() && mx > w-sc(64)
}

// scrubTo jumps the grid to the fraction of the timeline at screen y.
func (u *ui) scrubTo(my int32) {
	top := sc(gridTopBar)
	viewH := u.gridViewH()
	maxScroll := u.tl.contentH - viewH
	if maxScroll <= 0 {
		return
	}
	frac := float64(my-top) / float64(viewH-sc(40))
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	u.gridScrollY = int32(frac * float64(maxScroll))
}

// gridRowStep moves the cursor one thumbnail row up (dir<0) or down (dir>0),
// keeping the column, skipping header rows.
func (u *ui) gridRowStep(dir int) {
	u.ensureTimeline()
	tl := &u.tl
	if u.cursor < 0 || u.cursor >= len(tl.shotRow) {
		return
	}
	r := tl.shotRow[u.cursor]
	col := u.cursor - tl.rows[r].start
	rr := r + dir
	for rr >= 0 && rr < len(tl.rows) && tl.rows[rr].kind != tlCells {
		rr += dir
	}
	if rr < 0 || rr >= len(tl.rows) {
		return
	}
	t := tl.rows[rr].start + col
	if t >= tl.rows[rr].start+tl.rows[rr].n {
		t = tl.rows[rr].start + tl.rows[rr].n - 1
	}
	u.nav(t)
}

// gridDayJump moves the cursor to the first shot of the previous/next day.
func (u *ui) gridDayJump(dir int) {
	u.ensureTimeline()
	tl := &u.tl
	// find the current day group by its start
	cur := 0
	for i, ds := range tl.dayStarts {
		if ds <= u.cursor {
			cur = i
		} else {
			break
		}
	}
	next := cur + dir
	if next < 0 || next >= len(tl.dayStarts) {
		return
	}
	u.nav(tl.dayStarts[next])
}
