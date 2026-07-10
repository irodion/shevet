package tui

import "testing"

func TestLayoutCards_Shapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		w, h, n    int
		cols, rows int
	}{
		{"narrow terminal single column", 80, 24, 2, 1, 2},
		{"wide terminal two columns", 120, 30, 2, 2, 1},
		{"six panes on a big screen", 200, 50, 6, 4, 2},
		{"one pane", 200, 50, 1, 1, 1},
		{"degenerate viewport still lays out", 3, 2, 4, 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := layoutCards(tc.w, tc.h, tc.n)
			if l.cols != tc.cols || l.rows != tc.rows {
				t.Errorf("layout(%dx%d, %d panes) = %dx%d cards, want %dx%d",
					tc.w, tc.h, tc.n, l.cols, l.rows, tc.cols, tc.rows)
			}
			if l.cardW < 8 || l.cardH < minCardH {
				t.Errorf("card %dx%d below the render minimum", l.cardW, l.cardH)
			}
			if l.cardW > maxCardW || l.cardH > maxCardH {
				t.Errorf("card %dx%d above the caps", l.cardW, l.cardH)
			}
		})
	}
}

func TestLayoutCards_CardsFitTheViewport(t *testing.T) {
	l := layoutCards(120, 30, 4)
	if got := l.cols*l.cardW + (l.cols-1)*gutterX; got > 120-2*marginX {
		t.Errorf("card row spans %d columns, more than the %d available", got, 120-2*marginX)
	}
	if got := l.visibleRows*l.cardH + (l.visibleRows-1)*gutterY; got > 30-headerRows-footerRows {
		t.Errorf("visible cards span %d rows, more than the %d available", got, 30-headerRows-footerRows)
	}
}

func TestRowOffset_FollowsSelection(t *testing.T) {
	// 8 panes, 1 column, 3 visible rows.
	l := cardLayout{cols: 1, rows: 8, cardW: 40, cardH: 7, visibleRows: 3}

	if got := l.rowOffset(0); got != 0 {
		t.Errorf("offset at the top = %d, want 0", got)
	}
	if got := l.rowOffset(2); got != 0 {
		t.Errorf("offset with the selection still on screen = %d, want 0", got)
	}
	if got := l.rowOffset(4); got != 2 {
		t.Errorf("offset once the selection walks below = %d, want 2 (selected row on screen)", got)
	}
	if got := l.rowOffset(7); got != 5 {
		t.Errorf("offset at the bottom = %d, want 5 (the last 3 rows)", got)
	}
}
