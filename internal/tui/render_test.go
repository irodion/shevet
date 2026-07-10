package tui

import (
	"strings"
	"testing"

	"github.com/irodion/shevet/internal/grid"
)

func renderTest(w, h int, cells []grid.CellPatch, cursor grid.Cursor) string {
	g := grid.New(w, h)
	for _, p := range cells {
		g.Apply(p)
	}
	return renderPane(g, cursor)
}

func TestRenderPane_PlainContentAndBlanks(t *testing.T) {
	got := renderTest(4, 2, []grid.CellPatch{
		{X: 0, Y: 0, Cell: grid.Cell{Content: "a", Width: 1}},
		{X: 2, Y: 0, Cell: grid.Cell{Content: "b", Width: 1}},
		{X: 3, Y: 1, Cell: grid.Cell{Content: "c", Width: 1}},
	}, grid.Cursor{Hidden: true})

	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("rendered %d lines, want 2:\n%q", len(lines), got)
	}
	if plain := stripSGR(lines[0]); plain != "a b " {
		t.Errorf("row 0 = %q, want %q", plain, "a b ")
	}
	if plain := stripSGR(lines[1]); plain != "   c" {
		t.Errorf("row 1 = %q, want %q", plain, "   c")
	}
}

func TestRenderPane_WideCharSkipsSpacer(t *testing.T) {
	got := renderTest(4, 1, []grid.CellPatch{
		{X: 0, Y: 0, Cell: grid.Cell{Content: "你", Width: 2}},
		{X: 2, Y: 0, Cell: grid.Cell{Content: "A", Width: 1}},
	}, grid.Cursor{Hidden: true})

	if plain := stripSGR(got); plain != "你A " {
		t.Errorf("row = %q, want %q (spacer skipped, not double-rendered)", plain, "你A ")
	}
}

func TestRenderPane_StylesGroupIntoRuns(t *testing.T) {
	red := grid.Cell{Content: "r", Width: 1, FG: grid.RGB(255, 0, 0)}
	got := renderTest(3, 1, []grid.CellPatch{
		{X: 0, Y: 0, Cell: red},
		{X: 1, Y: 0, Cell: grid.Cell{Content: "s", Width: 1, FG: grid.RGB(255, 0, 0)}},
		{X: 2, Y: 0, Cell: grid.Cell{Content: "n", Width: 1}},
	}, grid.Cursor{Hidden: true})

	if want := "\x1b[0;38;2;255;0;0mrs"; !strings.Contains(got, want) {
		t.Errorf("adjacent same-style cells not grouped into one run:\n%q", got)
	}
	// The unstyled cell must not inherit the red run.
	if !strings.Contains(got, "\x1b[0mn") {
		t.Errorf("style not reset before the unstyled cell:\n%q", got)
	}
}

func TestRenderPane_AttributesEncode(t *testing.T) {
	got := renderTest(1, 1, []grid.CellPatch{
		{X: 0, Y: 0, Cell: grid.Cell{Content: "x", Width: 1, Attrs: grid.AttrBold | grid.AttrUnderline, BG: grid.RGB(0, 0, 255)}},
	}, grid.Cursor{Hidden: true})

	if want := "\x1b[0;1;4;48;2;0;0;255mx"; !strings.Contains(got, want) {
		t.Errorf("attribute encoding:\n got %q\n want it to contain %q", got, want)
	}
}

func TestRenderPane_CursorReversesItsCell(t *testing.T) {
	got := renderTest(2, 1, []grid.CellPatch{
		{X: 0, Y: 0, Cell: grid.Cell{Content: "a", Width: 1}},
	}, grid.Cursor{X: 0, Y: 0})

	if want := "\x1b[0;7ma"; !strings.Contains(got, want) {
		t.Errorf("visible cursor cell not reverse-videoed:\n%q", got)
	}

	hidden := renderTest(2, 1, []grid.CellPatch{
		{X: 0, Y: 0, Cell: grid.Cell{Content: "a", Width: 1}},
	}, grid.Cursor{X: 0, Y: 0, Hidden: true})
	if strings.Contains(hidden, "\x1b[0;7m") {
		t.Errorf("hidden cursor still rendered:\n%q", hidden)
	}
}

func TestRenderRegion_CropsAndPads(t *testing.T) {
	g := grid.New(6, 3)
	for i, r := range "abcdef" {
		g.Set(i, 0, grid.Cell{Content: string(r), Width: 1})
	}
	g.Set(0, 2, grid.Cell{Content: "z", Width: 1})

	// A 4x2 window on rows 1-2: row 1 is blank, row 2 clips to "z".
	got := renderRegion(g, grid.Cursor{Hidden: true}, 0, 1, 4, 2)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("rendered %d lines, want 2:\n%q", len(lines), got)
	}
	if plain := stripSGR(lines[0]); plain != "    " {
		t.Errorf("row 0 = %q, want 4 blanks", plain)
	}
	if plain := stripSGR(lines[1]); plain != "z   " {
		t.Errorf("row 1 = %q, want %q", plain, "z   ")
	}

	// A window wider and taller than the grid pads with blanks.
	got = renderRegion(g, grid.Cursor{Hidden: true}, 0, 0, 8, 4)
	lines = strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("rendered %d lines, want 4", len(lines))
	}
	if plain := stripSGR(lines[0]); plain != "abcdef  " {
		t.Errorf("padded row = %q, want %q", plain, "abcdef  ")
	}
}

func TestRenderRegion_WideCharAtTheEdgeBlanks(t *testing.T) {
	g := grid.New(4, 1)
	g.Set(0, 0, grid.Cell{Content: "A", Width: 1})
	g.Set(1, 0, grid.Cell{Content: "你", Width: 2})

	// A 2-wide window: the CJK cell would cross the right edge, so it
	// renders as a blank instead of overflowing the region.
	got := renderRegion(g, grid.Cursor{Hidden: true}, 0, 0, 2, 1)
	if plain := stripSGR(got); plain != "A " {
		t.Errorf("row = %q, want %q (wide cell blanked at the edge)", plain, "A ")
	}
}

func TestRenderRegion_CursorOutsideRegionNotShown(t *testing.T) {
	g := grid.New(4, 2)
	g.Set(0, 0, grid.Cell{Content: "a", Width: 1})

	// Cursor on row 1, region covers row 0 only: no reverse video anywhere.
	got := renderRegion(g, grid.Cursor{X: 0, Y: 1}, 0, 0, 4, 1)
	if strings.Contains(got, ";7m") {
		t.Errorf("cursor outside the region still rendered:\n%q", got)
	}
}

// stripSGR removes SGR escape sequences, leaving printable content.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
