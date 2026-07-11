package tui

// Card grid geometry. The dashboard reserves a header and footer line and
// tiles the rest with Pane cards: as many columns as the width comfortably
// carries, cards grown to fill, both dimensions bounded so a lone Pane on a
// huge terminal stays a card rather than a wasteland.
const (
	headerRows = 2 // brand line + one blank spacer
	footerRows = 1 // key hints
	marginX    = 2 // breathing room either side of the card grid
	gutterX    = 2 // columns between cards
	gutterY    = 1 // rows between card rows

	targetCardW = 46 // preferred outer card width — thumbnails stay legible
	maxCardW    = 84 // interior 82 ≥ a full default pane width
	minCardH    = 5  // borders + 3 thumbnail rows
	maxCardH    = 18 // interior 16 — enough to follow an Agent's tail
)

// cardLayout is the computed shape of the dashboard grid for one viewport
// and Pane count: how many card columns and rows, each card's outer size,
// how many card rows fit on screen at once (the rest scroll), and the body
// height the cards are placed in (viewport minus chrome — kept here so the
// card math and the view can never disagree about it).
type cardLayout struct {
	cols, rows   int
	cardW, cardH int
	visibleRows  int
	availH       int
}

// layoutCards computes the card grid for a viewport. It is total: any
// viewport, including a degenerate one, yields a usable layout (cards may
// then clip at the terminal edge, which beats dividing by zero).
func layoutCards(viewportW, viewportH, n int) cardLayout {
	n = max(n, 1)
	availW := max(viewportW-2*marginX, 1)
	availH := max(viewportH-headerRows-footerRows, minCardH)

	cols := clamp(availW/targetCardW, 1, n)
	cardW := min((availW-gutterX*(cols-1))/cols, maxCardW)
	rows := (n + cols - 1) / cols
	cardH := clamp((availH-gutterY*(rows-1))/rows, minCardH, maxCardH)
	visible := max((availH+gutterY)/(cardH+gutterY), 1)
	return cardLayout{
		cols:        cols,
		rows:        rows,
		cardW:       max(cardW, 8), // never thinner than its own corners
		cardH:       cardH,
		visibleRows: min(visible, rows),
		availH:      availH,
	}
}

// rowOffset is the first visible card row: 0 until the selection walks past
// the viewport, then just far enough that the selected row stays on screen.
// It is derived from the selection, not stored — scrolling has no state of
// its own to desync.
func (l cardLayout) rowOffset(selected int) int {
	selRow := selected / l.cols
	return clamp(selRow-l.visibleRows+1, 0, max(l.rows-l.visibleRows, 0))
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}
