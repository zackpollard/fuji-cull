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

	// keep cursor visible
	curRow := u.cursor / cols
	if curRow < u.gridTop {
		u.gridTop = curRow
	}
	if curRow >= u.gridTop+viewRows {
		u.gridTop = curRow - viewRows + 1
	}
	if u.gridTop < 0 {
		u.gridTop = 0
	}
	if u.gridTop > rows-1 {
		u.gridTop = rows - 1
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
			if tp, ok := u.app.ThumbPathIfReady(s.ID); ok && decodes < 12 {
				if te := u.thumbTex(s.ID, tp); te != nil {
					src := coverSrc(te.w, te.h, cellW, cellH)
					u.ren.Copy(te.tex, &src, &cell)
				} else {
					decodes++
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
