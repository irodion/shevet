// Package grid is Shevet's shared terminal-screen vocabulary: the Cell and
// Grid types that flow from the Server's emulator, through the damage differ
// and the wire, into the Client's Pane widget. It is a dependency-free leaf
// so every layer can speak it without import cycles.
//
// A Grid is a fixed-size matrix of Cells. The zero Cell is the default cell:
// no content (renders as a blank), no colors, no attributes. Keeping the
// default as the zero value is load-bearing for the wire: a full-grid sync
// only needs to carry the non-default cells.
package grid

// Attr is a bitmask of text attributes on a Cell.
type Attr uint32

// Text attributes. The set mirrors what both the emulator and mainstream
// terminals support; anything the emulator reports outside it is dropped.
const (
	AttrBold Attr = 1 << iota
	AttrFaint
	AttrItalic
	AttrUnderline
	AttrBlink
	AttrReverse
	AttrStrikethrough
)

// Color is a terminal color: the zero value means "terminal default", any
// other value is a 24-bit RGB color with a presence marker in bit 24 (so
// pure black is distinct from default). Indexed colors are resolved to RGB
// by the emulator's palette before they reach a Color.
type Color uint32

const colorPresent Color = 1 << 24

// RGB returns the Color for a 24-bit color.
func RGB(r, g, b uint8) Color {
	return colorPresent | Color(r)<<16 | Color(g)<<8 | Color(b)
}

// RGB reports the color's components; ok is false for the terminal default.
func (c Color) RGB() (r, g, b uint8, ok bool) {
	if c == 0 {
		return 0, 0, 0, false
	}
	return uint8(c >> 16), uint8(c >> 8), uint8(c), true
}

// Cell is one terminal screen cell.
//
// A wide grapheme (CJK, emoji) occupies Width columns: its Cell sits in the
// leftmost column and the covered columns to its right hold default
// ("spacer") Cells. Renderers advance by Width and never look at spacers.
type Cell struct {
	// Content is the cell's grapheme cluster; empty means a blank cell.
	Content string

	// Width is the number of columns the grapheme spans. Blank cells
	// (empty Content) span one column regardless of Width.
	Width int

	// FG and BG are the foreground and background colors.
	FG, BG Color

	// Attrs is the cell's text attributes.
	Attrs Attr
}

// IsDefault reports whether the cell is the default (zero) cell.
func (c Cell) IsDefault() bool {
	return c == Cell{}
}

// Cursor is the terminal cursor: a position plus visibility (DECTCEM).
type Cursor struct {
	X, Y   int
	Hidden bool
}

// Size is a grid size in cells, for APIs that pass one around (resize
// updates on the wire and in the pipeline).
type Size struct {
	W, H int
}

// CellPatch is one cell update: the damage unit carried on the wire.
type CellPatch struct {
	X, Y int
	Cell Cell
}

// Grid is a W×H matrix of Cells. The zero value is an empty 0×0 grid;
// construct with New. Grid is not safe for concurrent use.
type Grid struct {
	w, h  int
	cells []Cell
}

// New returns a Grid of the given size with every cell default.
// Negative dimensions are treated as zero.
func New(w, h int) *Grid {
	w, h = max(w, 0), max(h, 0)
	return &Grid{w: w, h: h, cells: make([]Cell, w*h)}
}

// Size returns the grid's width and height.
func (g *Grid) Size() (w, h int) {
	return g.w, g.h
}

// At returns the cell at (x, y); out-of-bounds positions read as default.
func (g *Grid) At(x, y int) Cell {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return Cell{}
	}
	return g.cells[y*g.w+x]
}

// Set writes the cell at (x, y); out-of-bounds positions are ignored.
func (g *Grid) Set(x, y int, c Cell) {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return
	}
	g.cells[y*g.w+x] = c
}

// Apply applies one patch to the grid.
func (g *Grid) Apply(p CellPatch) {
	g.Set(p.X, p.Y, p.Cell)
}

// Resize reshapes the grid to w×h, keeping the overlapping region's content
// and defaulting any newly exposed cells.
func (g *Grid) Resize(w, h int) {
	w, h = max(w, 0), max(h, 0)
	if w == g.w && h == g.h {
		return
	}
	cells := make([]Cell, w*h)
	for y := 0; y < min(h, g.h); y++ {
		copy(cells[y*w:y*w+min(w, g.w)], g.cells[y*g.w:y*g.w+min(w, g.w)])
	}
	g.w, g.h, g.cells = w, h, cells
}

// Snapshot returns patches for every non-default cell, in row-major order:
// the full-grid sync a fresh subscriber needs, minus the cells it already
// has by construction. The result is never nil, so it always reads as "a
// damage batch", even when empty.
func (g *Grid) Snapshot() []CellPatch {
	out := []CellPatch{}
	for i, c := range g.cells {
		if !c.IsDefault() {
			out = append(out, CellPatch{X: i % g.w, Y: i / g.w, Cell: c})
		}
	}
	return out
}

// Diff returns the patches that turn prev into next, in row-major order —
// the damage differ at the heart of the render pipeline. The grids must be
// the same size; resizes reset the shadow grid upstream and resync, so a
// size mismatch here is a programming error and panics.
func Diff(prev, next *Grid) []CellPatch {
	if prev.w != next.w || prev.h != next.h {
		panic("grid.Diff: size mismatch")
	}
	var out []CellPatch
	for i := range next.cells {
		if next.cells[i] != prev.cells[i] {
			out = append(out, CellPatch{X: i % next.w, Y: i / next.w, Cell: next.cells[i]})
		}
	}
	return out
}

// RowText returns row y's visible text: cell contents in column order with
// blanks as spaces, wide-cell spacers skipped, and trailing blanks trimmed.
// It is the human-readable projection used by tests and diagnostics.
func (g *Grid) RowText(y int) string {
	var b []byte
	for x := 0; x < g.w; {
		c := g.At(x, y)
		if c.Content == "" {
			b = append(b, ' ')
			x++
			continue
		}
		b = append(b, c.Content...)
		x += max(c.Width, 1)
	}
	end := len(b)
	for end > 0 && b[end-1] == ' ' {
		end--
	}
	return string(b[:end])
}
