package main

import (
	"fmt"

	"github.com/veandco/go-sdl2/sdl"
)

// Grid view: full-window thumbnail contact sheet. Same culling keys as the
// viewer; T/Escape return, Enter opens the shot in the viewer.
const (
	cellW, cellH, cellGap = 148, 100, 6
	gridPad               = 12
)

func (u *ui) gridCols() int {
	w, _ := u.win.GetSize()
	c := int((w - gridPad*2) / (cellW + cellGap))
	if c < 1 {
		c = 1
	}
	return c
}

func (u *ui) drawGrid() {
	w, h := u.win.GetSize()
	cols := u.gridCols()
	rows := (len(u.shots) + cols - 1) / cols
	rowPitch := int32(cellH + cellGap)
	viewRows := int(h-44-gridPad) / int(rowPitch)

	// Keep the cursor visible only when it MOVES — snapping every frame
	// fights manual scrolling and makes the rest of the grid unreachable.
	if u.cursor != u.lastGridCursor {
		u.lastGridCursor = u.cursor
		curRow := u.cursor / cols
		if curRow < u.gridTop {
			u.gridTop = curRow
		}
		if curRow >= u.gridTop+viewRows {
			u.gridTop = curRow - viewRows + 1
		}
	}
	maxTop := rows - viewRows
	if maxTop < 0 {
		maxTop = 0
	}
	if u.gridTop < 0 {
		u.gridTop = 0
	}
	if u.gridTop > maxTop {
		u.gridTop = maxTop
	}

	// retarget the thumbnail sweep at what's on screen
	firstVisible := u.gridTop * cols
	if firstVisible != u.lastHint {
		u.lastHint = firstVisible
		go u.app.SetThumbHint(firstVisible)
	}

	decodes := 0
	for r := 0; r <= viewRows; r++ {
		row := u.gridTop + r
		if row >= rows {
			break
		}
		for c := 0; c < cols; c++ {
			idx := row*cols + c
			if idx >= len(u.shots) {
				break
			}
			s := u.shots[idx]
			x := int32(gridPad + c*(cellW+cellGap))
			y := 44 + int32(r)*rowPitch
			cell := sdl.Rect{X: x, Y: y, W: cellW, H: cellH}
			u.fillRect(cell, colTickBG)
			if tp, ok := u.app.ThumbPathIfReady(s.ID); ok {
				// Cached textures always draw; only NEW synchronous decodes
				// are budgeted per frame (they cost main-thread time).
				te := u.thumbs.get(s.ID)
				if (te == nil || te.orient != u.orientAt(idx)) && decodes < 12 {
					te = u.thumbTex(s.ID, tp, u.orientAt(idx))
					decodes++
				}
				if te != nil {
					src := coverSrc(te.w, te.h, cellW, cellH)
					u.ren.Copy(te.tex, &src, &cell)
				}
			}
			if s.Kind == "video" {
				u.fillRect(sdl.Rect{X: x, Y: y, W: cellW, H: 3}, colAmber)
			}
			if d := u.decisions[s.ID]; d != "" {
				col := colKeep
				if d == "reject" {
					col = colReject
				}
				u.fillRect(sdl.Rect{X: x, Y: y + cellH - 4, W: cellW, H: 4}, col)
			}
			if idx == u.cursor {
				u.ren.SetDrawColor(colFG.R, colFG.G, colFG.B, 255)
				out := sdl.Rect{X: x - 2, Y: y - 2, W: cellW + 4, H: cellH + 4}
				u.ren.DrawRect(&out)
				in := sdl.Rect{X: x - 1, Y: y - 1, W: cellW + 2, H: cellH + 2}
				u.ren.DrawRect(&in)
			}
		}
	}
	// Preload beyond the fold with whatever decode budget the visible pass
	// left: nearest rows first, below before above (scrolling down
	// dominates), so revealed rows are already decoded instead of popping.
	for d := 1; d <= 14 && decodes < 12; d++ {
		for _, row := range [2]int{u.gridTop + viewRows + d, u.gridTop - d} {
			if row < 0 || row >= rows {
				continue
			}
			for c := 0; c < cols && decodes < 12; c++ {
				idx := row*cols + c
				if idx >= len(u.shots) {
					break
				}
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
	}

	u.text(u.fontSm, fmt.Sprintf("GRID  %d/%d   T/Esc close   Enter open   K/X/C cull", u.cursor+1, len(u.shots)),
		colDim, w/2, h-16, true)
}

// gridClick maps a mouse click to a cell index (-1 when outside).
func (u *ui) gridClick(mx, my int32) int {
	cols := u.gridCols()
	if my < 44 {
		return -1
	}
	c := int(mx-gridPad) / (cellW + cellGap)
	r := int(my-44) / (cellH + cellGap)
	if c < 0 || c >= cols {
		return -1
	}
	idx := (u.gridTop+r)*cols + c
	if idx < 0 || idx >= len(u.shots) {
		return -1
	}
	return idx
}
