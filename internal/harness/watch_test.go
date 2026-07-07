package harness

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/testutil"
)

// paneView consumes a WatchPane stream and folds it into a local grid — the
// e2e test client's model of what the Client renders.
type paneView struct {
	t     *testing.T
	watch *client.PaneWatch

	grid   *grid.Grid
	cursor grid.Cursor

	// batches records every damage batch after the initial sync, for the
	// wire-carries-damage-not-frames assertions.
	batches [][]grid.CellPatch
	synced  bool
	exited  bool
}

// watchPane opens the render stream for a pane, bounded by the test's
// standard wait timeout.
func watchPane(t *testing.T, c *client.Client, paneID string) *paneView {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	t.Cleanup(cancel)

	watch, err := c.WatchPane(ctx, paneID)
	if err != nil {
		t.Fatalf("WatchPane(%s): %v", paneID, err)
	}
	return &paneView{t: t, watch: watch, grid: grid.New(0, 0)}
}

// step folds the next stream update into the view.
func (v *paneView) step() {
	v.t.Helper()
	u, err := v.watch.Recv()
	if err != nil {
		v.t.Fatalf("Recv: %v (grid so far:\n%s)", err, v.dump())
	}
	switch {
	case u.Exited:
		v.exited = true
	case u.Resized != nil:
		v.grid = grid.New(u.Resized[0], u.Resized[1])
	case u.Damage != nil:
		if v.synced {
			v.batches = append(v.batches, u.Damage)
		}
		for _, p := range u.Damage {
			v.grid.Apply(p)
		}
		v.cursor = u.Cursor
		v.synced = true
	}
}

// waitFor steps the stream until the grid's text contains want. The stream
// context enforces the deadline — a wedged stream fails in Recv.
func (v *paneView) waitFor(want string) {
	v.t.Helper()
	for !strings.Contains(v.dump(), want) {
		v.step()
	}
}

// dump renders the grid's text content, one line per row.
func (v *paneView) dump() string {
	var b strings.Builder
	_, h := v.grid.Size()
	for y := 0; y < h; y++ {
		b.WriteString(v.grid.RowText(y))
		b.WriteByte('\n')
	}
	return b.String()
}

// TestWatchPane_LiveAgent is the acceptance test of issue #8: a scripted
// agent emits a known byte sequence — plain text, truecolor SGR, CJK — in a
// real sandbox pane, and the test client receives the expected final grid
// through the full pipeline (tmux control mode -> emulator -> damage differ
// -> wire).
func TestWatchPane_LiveAgent(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", strings.Join([]string{
		"print plain line",
		"print \x1b[38;2;255;100;0mORANGE\x1b[0m tail",
		"print wide 你好 cells",
		"prompt done > ",
		"await-line",
		"print after input",
		"prompt bye > ",
		"await-line",
	}, "\n"))

	view := watchPane(t, h.Client, pane)
	view.waitFor("done >")

	// The final grid, row by row.
	for row, want := range []string{"plain line", "ORANGE tail", "wide 你好 cells", "done >"} {
		if got := view.grid.RowText(row); got != want {
			t.Errorf("grid row %d = %q, want %q", row, got, want)
		}
	}

	// Styling and geometry survived the wire: truecolor on the SGR run,
	// default after reset, wide cells with their spacers.
	if c := view.grid.At(0, 1); c.FG != grid.RGB(255, 100, 0) {
		t.Errorf("ORANGE cell fg = %#x, want truecolor rgb(255,100,0)", c.FG)
	}
	if c := view.grid.At(7, 1); c.FG != 0 {
		t.Errorf("post-reset cell fg = %#x, want default", c.FG)
	}
	if c := view.grid.At(5, 2); c.Content != "你" || c.Width != 2 {
		t.Errorf("CJK cell = %+v, want 你 width 2", c)
	}

	// The cursor parks right after the prompt, visible.
	if want := (grid.Cursor{X: 7, Y: 3}); view.cursor != want {
		t.Errorf("cursor = %+v, want %+v", view.cursor, want)
	}

	// Damage, never full frames: drive the agent one step and verify the
	// increments stay proportional to the change, not to the screen.
	view.batches = nil
	h.Tmux.SendLine(t, pane, "go")
	view.waitFor("bye >")

	w, hgt := view.grid.Size()
	if len(view.batches) == 0 {
		t.Fatal("no incremental damage batches arrived")
	}
	for i, batch := range view.batches {
		if len(batch) >= w*hgt/2 {
			t.Errorf("batch %d carries %d cells on a %dx%d pane — that is a frame, not damage",
				i, len(batch), w, hgt)
		}
	}
}

func TestWatchPane_UnknownPaneIsNotFound(t *testing.T) {
	t.Parallel()
	h := Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	watch, err := h.Client.WatchPane(ctx, "%404")
	if err != nil {
		t.Fatalf("WatchPane: %v", err) // stream RPCs surface errors on Recv
	}
	_, err = watch.Recv()
	if status.Code(err) != codes.NotFound {
		t.Errorf("Recv error = %v, want NotFound", err)
	}
}

func TestWatchPane_PaneExitEndsStream(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.Tmux.NewWindow(t, "doomed", "sleep 86400")
	view := watchPane(t, h.Client, pane)
	view.step() // initial resize
	view.step() // initial sync damage

	h.Tmux.Run(t, "kill-pane", "-t", pane)
	for !view.exited {
		view.step()
	}
}

func TestWatchPane_TwoSubscribersSeeTheSamePane(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", "prompt ready > \nawait-line\n")

	a := watchPane(t, h.Client, pane)
	b := watchPane(t, h.Client, pane)
	a.waitFor("ready >")
	b.waitFor("ready >")

	if got, want := a.grid.RowText(0), b.grid.RowText(0); got != want {
		t.Errorf("subscribers disagree: %q vs %q", got, want)
	}
}
