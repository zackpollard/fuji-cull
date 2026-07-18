package main

import (
	"time"

	"github.com/zack/fuji-tools/internal/photo"
)

// Immich-style timeline layout for the grid view: shots are grouped into
// day sections under month bands, so the grid interleaves full-width date
// headers with rows of thumbnail cells. Cell size derives from the column
// count (fewer columns = bigger thumbs), so the grid always fills the width.
//
// The layout is O(shots) to build, so it is cached and only rebuilt when the
// column count, window width, UI scale, or the shot list changes. Rows are
// stored top-to-bottom in content-space Y; the draw path binary-searches the
// first visible row and per-shot rowIndex supports cursor navigation.

const (
	tlMonth uint8 = iota
	tlDay
	tlCells
)

type tlRow struct {
	kind     uint8
	y, h     int32
	label    string // headers only: display text
	key      string // month rows only: "2006-01" for the short rail label
	start, n int    // cells only: shot range [start, start+n)
}

type timeline struct {
	rows      []tlRow
	shotRow   []int // per shot: its cells-row index
	dayStarts []int // shot index of each day group's first shot
	contentH  int32
	cw, ch    int32 // cell size
	cols      int

	// cache keys
	keyCols  int
	keyW     int32
	keyScale float64
	keyLen   int
}

func dayKey(s *photo.Shot) string {
	if s.Date != "" {
		return s.Date
	}
	return s.Folder
}

func monthKeyOf(s *photo.Shot) string {
	if len(s.Date) >= 7 {
		return s.Date[:7]
	}
	return s.Folder
}

func prettyMonth(k string) string {
	if t, err := time.Parse("2006-01", k); err == nil {
		return t.Format("January 2006")
	}
	return k
}

func prettyMonthShort(k string) string {
	if t, err := time.Parse("2006-01", k); err == nil {
		return t.Format("Jan 2006")
	}
	return k
}

func prettyDay(k string) string {
	if t, err := time.Parse("2006-01-02", k); err == nil {
		return t.Format("Mon, Jan 2, 2006")
	}
	return k
}

// gridColsAuto is the width-derived column count (base cell aspect).
func (u *ui) gridColsAuto() int {
	w, _ := u.outSize()
	c := int((w - gridPad()*2 + cellGap()) / (cellW() + cellGap()))
	if c < 1 {
		c = 1
	}
	return c
}

// gridColCount is the effective column count: the user's choice, else auto.
func (u *ui) gridColCount() int {
	if u.userCols > 0 {
		return u.userCols
	}
	return u.gridColsAuto()
}

// cellSize derives the cell dimensions from the column count so the grid
// fills the width; height preserves the base aspect ratio.
func (u *ui) cellSize() (int32, int32) {
	w, _ := u.outSize()
	cols := int32(u.gridColCount())
	avail := w - gridPad()*2 - cellGap()*(cols-1)
	if avail < cols {
		avail = cols
	}
	cw := avail / cols
	ch := cw * cellH() / cellW()
	return cw, ch
}

// ensureTimeline rebuilds the layout if any cache key changed.
func (u *ui) ensureTimeline() {
	cols := u.gridColCount()
	w, _ := u.outSize()
	tl := &u.tl
	if tl.rows != nil && tl.keyCols == cols && tl.keyW == w &&
		tl.keyScale == userScale && tl.keyLen == len(u.shots) {
		return
	}
	cw, ch := u.cellSize()
	monthH := sc(46)
	dayH := sc(28)
	rowPitch := ch + cellGap()

	rows := make([]tlRow, 0, len(u.shots)/max(cols, 1)+64)
	shotRow := make([]int, len(u.shots))
	dayStarts := []int{}

	var y int32
	curMonth, curDay := "\x00", "\x00"
	i := 0
	for i < len(u.shots) {
		s := u.shots[i]
		month := monthKeyOf(s)
		day := dayKey(s)
		if month != curMonth {
			curMonth = month
			curDay = "\x00"
			rows = append(rows, tlRow{kind: tlMonth, y: y, h: monthH, label: prettyMonth(month), key: month})
			y += monthH
		}
		if day != curDay {
			curDay = day
			rows = append(rows, tlRow{kind: tlDay, y: y, h: dayH, label: prettyDay(day)})
			y += dayH
			dayStarts = append(dayStarts, i)
		}
		// gather the whole day group, then lay it out in rows of `cols`
		start := i
		for i < len(u.shots) && dayKey(u.shots[i]) == curDay {
			i++
		}
		for c0 := start; c0 < i; c0 += cols {
			n := cols
			if c0+n > i {
				n = i - c0
			}
			ri := len(rows)
			rows = append(rows, tlRow{kind: tlCells, y: y, h: rowPitch, start: c0, n: n})
			for k := 0; k < n; k++ {
				shotRow[c0+k] = ri
			}
			y += rowPitch
		}
	}

	tl.rows = rows
	tl.shotRow = shotRow
	tl.dayStarts = dayStarts
	tl.contentH = y
	tl.cw, tl.ch = cw, ch
	tl.cols = cols
	tl.keyCols, tl.keyW, tl.keyScale, tl.keyLen = cols, w, userScale, len(u.shots)
}

// firstVisibleRow binary-searches the first row whose bottom is past scrollY.
func (tl *timeline) firstVisibleRow(scrollY int32) int {
	lo, hi := 0, len(tl.rows)
	for lo < hi {
		mid := (lo + hi) / 2
		if tl.rows[mid].y+tl.rows[mid].h <= scrollY {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// rowAt returns the row index whose span contains content-space y, or -1.
func (tl *timeline) rowAt(y int32) int {
	if y < 0 || len(tl.rows) == 0 || y >= tl.contentH {
		return -1
	}
	r := tl.firstVisibleRow(y)
	if r >= len(tl.rows) {
		return -1
	}
	return r
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
