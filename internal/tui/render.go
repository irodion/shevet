package tui

import (
	"strconv"
	"strings"

	"github.com/irodion/shevet/internal/grid"
)

// renderPane draws a whole Pane grid as styled terminal lines for the Bubble
// Tea renderer (which handles cell diffing against the real terminal).
func renderPane(g *grid.Grid, cursor grid.Cursor) string {
	w, h := g.Size()
	return renderRegion(g, cursor, 0, 0, w, h)
}

// renderRegion draws the w×h region of g anchored at (x0, y0) — the whole
// grid for a focused Pane, a cropped live thumbnail for a card. Styles are
// emitted as raw SGR sequences grouped into runs — a per-cell lipgloss style
// would re-emit codes for every cell.
//
// Every row is rendered to exactly w columns: positions outside the grid
// read as default cells, so a region larger than the Pane pads with blanks
// and a smaller one clips. A wide grapheme that would cross the region's
// right edge is rendered as blanks instead of overflowing it. The cursor,
// when visible and inside the region, is shown by reverse-videoing its cell:
// the real terminal cursor belongs to the dashboard, not to the watched
// Pane.
func renderRegion(g *grid.Grid, cursor grid.Cursor, x0, y0, w, h int) string {
	var b strings.Builder
	b.Grow(w * h * 2)

	for ry := 0; ry < h; ry++ {
		y := y0 + ry
		var last grid.Cell // zero: the default style an SGR reset yields
		for rx := 0; rx < w; {
			x := x0 + rx
			c := g.At(x, y)
			if !cursor.Hidden && x == cursor.X && y == cursor.Y {
				c.Attrs ^= grid.AttrReverse
			}
			if c.Content != "" && rx+max(c.Width, 1) > w {
				// The grapheme would cross the right edge: blank the cell,
				// keeping its style, rather than overflow the region.
				c.Content = ""
			}
			if !sameStyle(c, last) {
				writeSGR(&b, c)
			}
			last = c

			if c.Content == "" {
				b.WriteByte(' ')
				rx++
				continue
			}
			b.WriteString(c.Content)
			rx += max(c.Width, 1)
		}
		b.WriteString("\x1b[0m")
		if ry < h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// sameStyle reports whether two cells share fg, bg, and attributes.
func sameStyle(a, b grid.Cell) bool {
	return a.FG == b.FG && a.BG == b.BG && a.Attrs == b.Attrs
}

// sgrAttrs maps grid attributes to their SGR parameter.
var sgrAttrs = [...]struct {
	attr grid.Attr
	code string
}{
	{grid.AttrBold, "1"},
	{grid.AttrFaint, "2"},
	{grid.AttrItalic, "3"},
	{grid.AttrUnderline, "4"},
	{grid.AttrBlink, "5"},
	{grid.AttrReverse, "7"},
	{grid.AttrStrikethrough, "9"},
}

// writeSGR emits a reset followed by the cell's full style. Building from
// reset keeps the encoder stateless; run-grouping keeps it cheap.
func writeSGR(b *strings.Builder, c grid.Cell) {
	b.WriteString("\x1b[0")
	for _, m := range sgrAttrs {
		if c.Attrs&m.attr != 0 {
			b.WriteByte(';')
			b.WriteString(m.code)
		}
	}
	if r, g, bl, ok := c.FG.RGB(); ok {
		b.WriteString(";38;2;")
		writeRGB(b, r, g, bl)
	}
	if r, g, bl, ok := c.BG.RGB(); ok {
		b.WriteString(";48;2;")
		writeRGB(b, r, g, bl)
	}
	b.WriteByte('m')
}

func writeRGB(b *strings.Builder, r, g, bl uint8) {
	b.WriteString(strconv.Itoa(int(r)))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(int(g)))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(int(bl)))
}
