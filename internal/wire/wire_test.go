package wire

import (
	"testing"

	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
)

// TestPane_RoundTrip guards the two mapping directions against drifting
// apart: every domain field must survive domain -> proto -> domain.
func TestPane_RoundTrip(t *testing.T) {
	in := herd.Pane{ID: "%1", Title: "agent-a"}

	if got := PaneFromProto(PaneToProto(in)); got != in {
		t.Errorf("round trip mangled the Pane: got %+v, want %+v", got, in)
	}
}

func TestPatch_RoundTrip(t *testing.T) {
	in := grid.CellPatch{
		X: 7, Y: 3,
		Cell: grid.Cell{
			Content: "你",
			Width:   2,
			FG:      grid.RGB(255, 100, 0),
			BG:      grid.RGB(0, 0, 1),
			Attrs:   grid.AttrBold | grid.AttrUnderline,
		},
	}

	if got := PatchFromProto(PatchToProto(in)); got != in {
		t.Errorf("round trip mangled the patch: got %+v, want %+v", got, in)
	}
}

func TestDamage_RoundTrip(t *testing.T) {
	cells := []grid.CellPatch{
		{X: 1, Y: 2, Cell: grid.Cell{Content: "a", Width: 1, FG: grid.RGB(1, 2, 3)}},
		{X: 3, Y: 2, Cell: grid.Cell{}},
	}
	cursor := grid.Cursor{X: 4, Y: 2, Hidden: true}

	gotCells, gotCursor := DamageFromProto(DamageToProto(cells, cursor))
	if len(gotCells) != len(cells) || gotCells[0] != cells[0] || gotCells[1] != cells[1] {
		t.Errorf("round trip mangled the cells: got %+v, want %+v", gotCells, cells)
	}
	if gotCursor != cursor {
		t.Errorf("round trip mangled the cursor: got %+v, want %+v", gotCursor, cursor)
	}
}

func TestCursor_RoundTrip(t *testing.T) {
	in := grid.Cursor{X: 12, Y: 5, Hidden: true}

	if got := CursorFromProto(CursorToProto(in)); got != in {
		t.Errorf("round trip mangled the cursor: got %+v, want %+v", got, in)
	}
}
