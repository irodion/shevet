package grid

import (
	"reflect"
	"testing"
)

// fill writes s's runes as single-width cells starting at (x, y).
func fill(g *Grid, x, y int, s string, c Cell) {
	for i, r := range []rune(s) {
		cell := c
		cell.Content = string(r)
		cell.Width = 1
		g.Set(x+i, y, cell)
	}
}

func TestColor_RGBRoundTrip(t *testing.T) {
	r, g, b, ok := RGB(255, 100, 0).RGB()
	if !ok || r != 255 || g != 100 || b != 0 {
		t.Errorf("RGB(255,100,0).RGB() = %d,%d,%d,%v", r, g, b, ok)
	}

	if _, _, _, ok := Color(0).RGB(); ok {
		t.Error("default Color reported ok = true")
	}

	// Pure black must be distinct from the terminal default.
	if RGB(0, 0, 0) == 0 {
		t.Error("RGB(0,0,0) equals the default Color")
	}
}

func TestGrid_AtSetOutOfBoundsAreSafe(t *testing.T) {
	g := New(2, 2)
	for _, p := range [][2]int{{-1, 0}, {0, -1}, {2, 0}, {0, 2}} {
		g.Set(p[0], p[1], Cell{Content: "x", Width: 1})
		if got := g.At(p[0], p[1]); !got.IsDefault() {
			t.Errorf("At(%d,%d) after out-of-bounds Set = %+v, want default", p[0], p[1], got)
		}
	}
}

func TestGrid_ResizeKeepsOverlap(t *testing.T) {
	g := New(4, 2)
	fill(g, 0, 0, "abcd", Cell{})
	fill(g, 0, 1, "wxyz", Cell{})

	g.Resize(2, 3)
	if w, h := g.Size(); w != 2 || h != 3 {
		t.Fatalf("Size after Resize = %dx%d, want 2x3", w, h)
	}
	if got := g.At(1, 1); got.Content != "x" {
		t.Errorf("At(1,1) = %q, want %q", got.Content, "x")
	}
	if got := g.At(0, 2); !got.IsDefault() {
		t.Errorf("newly exposed cell = %+v, want default", got)
	}
}

// TestDiff_GridPairs is the damage differ's golden suite: pairs of grids and
// the exact patch list that turns the first into the second.
func TestDiff_GridPairs(t *testing.T) {
	red := RGB(255, 0, 0)

	for _, tc := range []struct {
		name string
		prev func() *Grid
		next func() *Grid
		want []CellPatch
	}{
		{
			name: "identical grids produce no damage",
			prev: func() *Grid { g := New(3, 2); fill(g, 0, 0, "hi", Cell{}); return g },
			next: func() *Grid { g := New(3, 2); fill(g, 0, 0, "hi", Cell{}); return g },
			want: nil,
		},
		{
			name: "single new cell",
			prev: func() *Grid { return New(3, 2) },
			next: func() *Grid { g := New(3, 2); g.Set(1, 1, Cell{Content: "A", Width: 1}); return g },
			want: []CellPatch{{X: 1, Y: 1, Cell: Cell{Content: "A", Width: 1}}},
		},
		{
			name: "style-only change is damage",
			prev: func() *Grid { g := New(2, 1); g.Set(0, 0, Cell{Content: "A", Width: 1}); return g },
			next: func() *Grid {
				g := New(2, 1)
				g.Set(0, 0, Cell{Content: "A", Width: 1, FG: red, Attrs: AttrBold})
				return g
			},
			want: []CellPatch{{X: 0, Y: 0, Cell: Cell{Content: "A", Width: 1, FG: red, Attrs: AttrBold}}},
		},
		{
			name: "cell cleared back to default",
			prev: func() *Grid { g := New(2, 1); g.Set(1, 0, Cell{Content: "B", Width: 1}); return g },
			next: func() *Grid { return New(2, 1) },
			want: []CellPatch{{X: 1, Y: 0, Cell: Cell{}}},
		},
		{
			name: "wide char replacing two narrow cells",
			prev: func() *Grid { g := New(3, 1); fill(g, 0, 0, "AB", Cell{}); return g },
			next: func() *Grid { g := New(3, 1); g.Set(0, 0, Cell{Content: "你", Width: 2}); return g },
			want: []CellPatch{
				{X: 0, Y: 0, Cell: Cell{Content: "你", Width: 2}},
				{X: 1, Y: 0, Cell: Cell{}}, // spacer under the wide char
			},
		},
		{
			name: "row scroll damages both rows",
			prev: func() *Grid {
				g := New(2, 2)
				fill(g, 0, 0, "aa", Cell{})
				fill(g, 0, 1, "bb", Cell{})
				return g
			},
			next: func() *Grid { g := New(2, 2); fill(g, 0, 0, "bb", Cell{}); return g },
			want: []CellPatch{
				{X: 0, Y: 0, Cell: Cell{Content: "b", Width: 1}},
				{X: 1, Y: 0, Cell: Cell{Content: "b", Width: 1}},
				{X: 0, Y: 1, Cell: Cell{}},
				{X: 1, Y: 1, Cell: Cell{}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Diff(tc.prev(), tc.next())
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Diff patches:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

func TestDiff_SizeMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Diff with mismatched sizes did not panic")
		}
	}()
	Diff(New(2, 2), New(3, 2))
}

func TestSnapshot_CarriesOnlyNonDefaultCells(t *testing.T) {
	g := New(3, 2)
	g.Set(2, 0, Cell{Content: "X", Width: 1})
	g.Set(0, 1, Cell{Content: "Y", Width: 1, BG: RGB(0, 0, 255)})

	want := []CellPatch{
		{X: 2, Y: 0, Cell: Cell{Content: "X", Width: 1}},
		{X: 0, Y: 1, Cell: Cell{Content: "Y", Width: 1, BG: RGB(0, 0, 255)}},
	}
	if got := g.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("Snapshot:\n got  %+v\n want %+v", got, want)
	}

	// A snapshot applied to a fresh grid reproduces the original.
	dup := New(3, 2)
	for _, p := range g.Snapshot() {
		dup.Apply(p)
	}
	if !reflect.DeepEqual(dup, g) {
		t.Error("applying Snapshot to a fresh grid did not reproduce the original")
	}
}

func TestRowText(t *testing.T) {
	g := New(8, 2)
	g.Set(0, 0, Cell{Content: "你", Width: 2})
	g.Set(2, 0, Cell{Content: "A", Width: 1})
	g.Set(4, 0, Cell{Content: "B", Width: 1})

	if got := g.RowText(0); got != "你A B" {
		t.Errorf("RowText(0) = %q, want %q", got, "你A B")
	}
	if got := g.RowText(1); got != "" {
		t.Errorf("RowText(1) = %q, want empty", got)
	}
}
