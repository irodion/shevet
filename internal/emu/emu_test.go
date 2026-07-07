package emu

import (
	"testing"

	"github.com/irodion/shevet/internal/grid"
)

// The byte-stream fixtures reuse the vt-emulator selection corpus
// (docs/research/vtbench): the sequences that decided ADR-0006 keep guarding
// the adapter — and any future emulator swap — here.

// snap writes the stream to a fresh emulator and returns the resulting grid.
func snap(t *testing.T, w, h int, stream string) (Emulator, *grid.Grid) {
	t.Helper()
	e := New(w, h)
	if _, err := e.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	g := grid.New(0, 0)
	e.Snapshot(g)
	return e, g
}

func TestSnapshot_PlainText(t *testing.T) {
	_, g := snap(t, 20, 6, "hi")
	if got := g.At(0, 0); got.Content != "h" || got.Width != 1 {
		t.Errorf("cell(0,0) = %+v, want h width 1", got)
	}
	if got := g.At(1, 0).Content; got != "i" {
		t.Errorf("cell(1,0) = %q, want i", got)
	}
}

func TestSnapshot_TruecolorSGR(t *testing.T) {
	_, g := snap(t, 20, 6, "\x1b[38;2;255;100;0mX")
	got := g.At(0, 0)
	if got.Content != "X" || got.FG != grid.RGB(255, 100, 0) {
		t.Errorf("cell(0,0) = %+v, want X with fg rgb(255,100,0)", got)
	}
}

func TestSnapshot_256ColorResolvesToPaletteRGB(t *testing.T) {
	_, g := snap(t, 20, 6, "\x1b[38;5;196mY")
	got := g.At(0, 0)
	if got.Content != "Y" || got.FG != grid.RGB(255, 0, 0) {
		t.Errorf("cell(0,0) = %+v, want Y with fg rgb(255,0,0) (xterm 196)", got)
	}
}

func TestWrite_EscapeSplitAcrossWrites(t *testing.T) {
	e := New(20, 6)
	for _, chunk := range []string{"\x1b[38;2;", "0;255;0mZ"} {
		if _, err := e.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}
	g := grid.New(0, 0)
	e.Snapshot(g)
	got := g.At(0, 0)
	if got.Content != "Z" || got.FG != grid.RGB(0, 255, 0) {
		t.Errorf("cell(0,0) = %+v, want Z with fg rgb(0,255,0)", got)
	}
}

func TestSnapshot_CJKWideCell(t *testing.T) {
	_, g := snap(t, 20, 6, "你A")
	if got := g.At(0, 0); got.Content != "你" || got.Width != 2 {
		t.Errorf("cell(0,0) = %+v, want 你 width 2", got)
	}
	if got := g.At(1, 0); !got.IsDefault() {
		t.Errorf("spacer cell(1,0) = %+v, want default", got)
	}
	if got := g.At(2, 0).Content; got != "A" {
		t.Errorf("cell(2,0) = %q, want A", got)
	}
}

func TestSnapshot_EmojiZWJCluster(t *testing.T) {
	_, g := snap(t, 20, 6, "👩‍🚀B")
	if got := g.At(0, 0); got.Content != "👩‍🚀" || got.Width != 2 {
		t.Errorf("cell(0,0) = %+v, want the full ZWJ cluster, width 2", got)
	}
	if got := g.At(2, 0).Content; got != "B" {
		t.Errorf("cell(2,0) = %q, want B", got)
	}
}

func TestSnapshot_Attributes(t *testing.T) {
	_, g := snap(t, 20, 6, "\x1b[1;4mS")
	got := g.At(0, 0)
	if got.Attrs != grid.AttrBold|grid.AttrUnderline {
		t.Errorf("attrs = %b, want bold|underline", got.Attrs)
	}
}

func TestSnapshot_BlankCellsAreDefault(t *testing.T) {
	_, g := snap(t, 20, 6, "a")
	// Everything except the written cell must be the zero Cell, so full-grid
	// syncs stay proportional to content, not to screen area.
	if patches := g.Snapshot(); len(patches) != 1 {
		t.Errorf("non-default cells = %d, want 1: %+v", len(patches), patches)
	}
}

func TestCursor_MotionAndVisibility(t *testing.T) {
	e, _ := snap(t, 20, 6, "\x1b[5;10H")
	if got := e.Cursor(); got.X != 9 || got.Y != 4 || got.Hidden {
		t.Errorf("cursor after CUP 5;10 = %+v, want (9,4) visible", got)
	}

	if _, err := e.Write([]byte("\x1b[?25l")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := e.Cursor(); !got.Hidden {
		t.Errorf("cursor after DECTCEM reset = %+v, want hidden", got)
	}
	if _, err := e.Write([]byte("\x1b[?25h")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := e.Cursor(); got.Hidden {
		t.Errorf("cursor after DECTCEM set = %+v, want visible", got)
	}
}

func TestResize_ContentSurvives(t *testing.T) {
	e, _ := snap(t, 20, 6, "RESIZE-TEST")
	e.Resize(10, 3)
	e.Resize(30, 8)
	if w, h := e.Size(); w != 30 || h != 8 {
		t.Fatalf("Size = %dx%d, want 30x8", w, h)
	}

	g := grid.New(0, 0)
	e.Snapshot(g)
	if w, h := g.Size(); w != 30 || h != 8 {
		t.Fatalf("snapshot size = %dx%d, want 30x8", w, h)
	}
	if got := g.At(0, 0).Content; got != "R" {
		t.Errorf("cell(0,0) after resizes = %q, want R", got)
	}
}

func TestSnapshot_CursorMotionOverwrite(t *testing.T) {
	// Write, jump home, overwrite: the grid must reflect the overwrite —
	// this is the shape of every status line and progress meter.
	_, g := snap(t, 20, 6, "aaaa\x1b[1;1Hbb")
	if got := g.At(0, 0).Content + g.At(1, 0).Content + g.At(2, 0).Content + g.At(3, 0).Content; got != "bbaa" {
		t.Errorf("row 0 = %q, want bbaa", got)
	}
}
