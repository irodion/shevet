// Package emu wraps the VT emulator behind Shevet's own interface, per
// ADR-0006: the concrete library (charmbracelet/x/vt, pinned by commit) is
// experimental and unversioned, so the rest of the Server programs against
// this small surface and the library stays swappable (midterm is the
// designated fallback).
//
// An Emulator is NOT safe for concurrent use: one goroutine owns each
// instance end-to-end (the per-Pane pipeline), which also sidesteps x/vt's
// open Close() race (charmbracelet/x#879).
package emu

import (
	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"

	"github.com/irodion/shevet/internal/grid"
)

// Emulator is Shevet's view of a terminal emulator: bytes in, grid out.
type Emulator interface {
	// Write feeds pane output bytes to the emulator. Escape sequences may
	// be split across calls.
	Write(p []byte) (int, error)

	// Resize resizes the screen.
	Resize(w, h int)

	// Size returns the current screen size.
	Size() (w, h int)

	// Snapshot copies the current screen into dst, resizing it as needed.
	Snapshot(dst *grid.Grid)

	// Cursor returns the current cursor state.
	Cursor() grid.Cursor
}

// vtEmulator adapts charmbracelet/x/vt to the Emulator interface.
type vtEmulator struct {
	term *vt.Emulator

	// cursorHidden tracks DECTCEM via the emulator's callback: x/vt exposes
	// cursor position but not visibility as a getter.
	cursorHidden bool
}

// New returns an x/vt-backed Emulator with a w×h screen.
func New(w, h int) Emulator {
	e := &vtEmulator{term: vt.NewEmulator(w, h)}
	e.term.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { e.cursorHidden = !visible },
	})
	return e
}

func (e *vtEmulator) Write(p []byte) (int, error) {
	return e.term.Write(p)
}

func (e *vtEmulator) Resize(w, h int) {
	e.term.Resize(w, h)
}

func (e *vtEmulator) Size() (w, h int) {
	return e.term.Width(), e.term.Height()
}

func (e *vtEmulator) Cursor() grid.Cursor {
	pos := e.term.CursorPosition()
	return grid.Cursor{X: pos.X, Y: pos.Y, Hidden: e.cursorHidden}
}

func (e *vtEmulator) Snapshot(dst *grid.Grid) {
	w, h := e.Size()
	dst.Resize(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, convertCell(e.term.CellAt(x, y)))
		}
	}
}

// convertCell maps an x/vt cell to a grid.Cell, normalizing unstyled blanks
// to the default cell (the wire carries only non-default cells on sync).
func convertCell(c *uv.Cell) grid.Cell {
	if c == nil {
		return grid.Cell{}
	}
	out := grid.Cell{
		Content: c.Content,
		Width:   c.Width,
		FG:      convertColor(c.Style.Fg),
		BG:      convertColor(c.Style.Bg),
		Attrs:   convertAttrs(c.Style),
	}
	if (out.Content == "" || out.Content == " ") && out.FG == 0 && out.BG == 0 && out.Attrs == 0 {
		return grid.Cell{}
	}
	return out
}

// convertColor resolves an emulator color to grid's RGB Color. Indexed and
// named colors are resolved through their palette RGB values; nil is the
// terminal default.
func convertColor(c interface{ RGBA() (r, g, b, a uint32) }) grid.Color {
	if c == nil {
		return 0
	}
	r, g, b, _ := c.RGBA()
	return grid.RGB(uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// convertAttrs maps x/vt style bits onto grid attributes. Underline is a
// separate style dimension in x/vt (it has curl/double variants); any
// underline style maps to the single AttrUnderline bit.
func convertAttrs(s uv.Style) grid.Attr {
	var a grid.Attr
	for _, m := range [...]struct {
		from uint8
		to   grid.Attr
	}{
		{uv.AttrBold, grid.AttrBold},
		{uv.AttrFaint, grid.AttrFaint},
		{uv.AttrItalic, grid.AttrItalic},
		{uv.AttrBlink, grid.AttrBlink},
		{uv.AttrReverse, grid.AttrReverse},
		{uv.AttrStrikethrough, grid.AttrStrikethrough},
	} {
		if s.Attrs&m.from != 0 {
			a |= m.to
		}
	}
	if s.Underline != uv.UnderlineNone {
		a |= grid.AttrUnderline
	}
	return a
}
