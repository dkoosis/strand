package forest

import "sort"

// Rect is a treemap cell in normalized 0–100 percentage space. The server lays
// the treemap out in this unit space and the template emits the coordinates as
// CSS percentages, so the browser scales the map to any container with no
// client-side layout pass (spec D8: server renders, minimal JS).
type Rect struct {
	X, Y, W, H float64
}

// cell pairs an input index with its weight so squarify can return rects in the
// caller's original order regardless of the internal sort.
type cell struct {
	idx int
	v   float64
}

// box is a working rectangle in absolute (w×h) units during layout.
type box struct {
	x, y, w, h float64
}

// unit is the side of the normalized square the treemap lays out in; rects come
// back as 0–100 percentages so CSS scales them to any container.
const unit = 100.0

// squarify lays out weighted items into the normalized unit square using the
// squarified treemap algorithm (Bruls, Huizing, van Wijk), favoring near-square
// tiles so area reads as magnitude at a glance. It returns one Rect per input
// value, in input order. Zero or negative weights collapse to zero-area cells.
func squarify(values []float64) []Rect {
	out := make([]Rect, len(values))
	if len(values) == 0 {
		return out
	}
	cells, total := newCells(values)
	if total <= 0 {
		return out
	}
	// Largest first is what makes the layout squarish.
	sort.SliceStable(cells, func(a, b int) bool { return cells[a].v > cells[b].v })

	scale := (unit * unit) / total // weight → area
	r := box{0, 0, unit, unit}
	for i := 0; i < len(cells); {
		horiz, length := orient(r)
		n := rowLen(cells[i:], length, scale)
		row := cells[i : i+n]
		placeRow(out, row, r, horiz, length, scale)
		r = advance(r, rowArea(row, scale)/length, horiz)
		i += n
	}
	return out
}

// newCells builds the working cells and returns their total weight; negative
// weights are floored to zero.
func newCells(values []float64) ([]cell, float64) {
	var total float64
	cells := make([]cell, 0, len(values))
	for i, v := range values {
		if v < 0 {
			v = 0
		}
		total += v
		cells = append(cells, cell{idx: i, v: v})
	}
	return cells, total
}

// orient returns whether the next row lays out horizontally and the length of
// the side it spans — always the shorter side, which keeps tiles square.
func orient(r box) (bool, float64) {
	if r.w < r.h {
		return true, r.w
	}
	return false, r.h
}

// rowLen grows a row from the front of cells while doing so keeps the worst
// aspect ratio from getting worse, and returns its length (at least one).
func rowLen(cells []cell, length, scale float64) int {
	n := 1
	for n < len(cells) && worst(cells[:n], length, scale) >= worst(cells[:n+1], length, scale) {
		n++
	}
	return n
}

// placeRow writes the rects for one row, packed along the shorter side and sized
// by each cell's share, converting to 0–100 percentages on the way out.
func placeRow(out []Rect, row []cell, r box, horiz bool, length, scale float64) {
	area := rowArea(row, scale)
	if area <= 0 {
		return // a row of zero-weight cells stays at zero area
	}
	thick := area / length
	off := r.y
	if horiz {
		off = r.x
	}
	for _, c := range row {
		frac := (c.v * scale) / area * length
		if horiz {
			out[c.idx] = pct(off, r.y, frac, thick)
		} else {
			out[c.idx] = pct(r.x, off, thick, frac)
		}
		off += frac
	}
}

// advance peels the placed row off the rectangle, leaving the remainder.
func advance(r box, thick float64, horiz bool) box {
	if horiz {
		r.y += thick
		r.h -= thick
	} else {
		r.x += thick
		r.w -= thick
	}
	return r
}

func rowArea(row []cell, scale float64) float64 {
	var a float64
	for _, c := range row {
		a += c.v * scale
	}
	return a
}

// pct converts an absolute cell in the unit square into 0–100 percentages.
func pct(x, y, cw, ch float64) Rect {
	return Rect{X: x / unit * 100, Y: y / unit * 100, W: cw / unit * 100, H: ch / unit * 100}
}

// worst returns the worst aspect ratio in a row laid along a side of the given
// length — the squarify cost function (lower is squarer).
func worst(row []cell, length, scale float64) float64 {
	var sum, mx, mn float64
	mn = -1
	for _, c := range row {
		a := c.v * scale
		sum += a
		if a > mx {
			mx = a
		}
		if mn < 0 || a < mn {
			mn = a
		}
	}
	if sum <= 0 || mn <= 0 {
		return 1e18 // degenerate row: never preferred
	}
	l2, s2 := length*length, sum*sum
	r1, r2 := l2*mx/s2, s2/(l2*mn)
	if r1 > r2 {
		return r1
	}
	return r2
}
