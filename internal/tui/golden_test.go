package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
	"github.com/irodion/shevet/internal/testutil"
)

// The golden tests pin the dashboard's structure and content — not its
// colors (the acceptance criterion of #11): frames are ANSI-stripped before
// comparison, so a theme change never invalidates them but a layout or
// content regression does. Regenerate with `go test ./internal/tui -update`.

// goldenFrame normalizes a rendered frame for golden comparison: ANSI
// stripped, trailing spaces trimmed (so editors cannot silently corrupt the
// fixture), and newline-terminated.
func goldenFrame(content string) []byte {
	lines := strings.Split(ansi.Strip(content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// rowCellsAt builds damage patches for one row of text starting at column
// x0, advancing by each rune's display width so wide (CJK) content lands
// the way the emulator lays it out.
func rowCellsAt(x0, y int, text string) []grid.CellPatch {
	var out []grid.CellPatch
	x := x0
	for _, r := range text {
		w := max(ansi.StringWidth(string(r)), 1)
		out = append(out, grid.CellPatch{X: x, Y: y, Cell: grid.Cell{Content: string(r), Width: w}})
		x += w
	}
	return out
}

// screenUpdate assembles one damage batch: rows of text from the top-left,
// plus the cursor.
func screenUpdate(cursor grid.Cursor, rows ...string) client.PaneUpdate {
	var patches []grid.CellPatch
	for y, row := range rows {
		patches = append(patches, rowCellsAt(0, y, row)...)
	}
	return client.PaneUpdate{Damage: patches, Cursor: cursor}
}

// goldenConn is the fixture Herd: three Panes with deterministic screens —
// an agent mid-task, a test run with wide CJK cells, an editor. Streams
// park once their script is played, like live Panes with nothing to say.
func goldenConn(t *testing.T) *fakeConn {
	t.Helper()
	park := make(chan struct{})
	t.Cleanup(func() { close(park) })

	agent := screenUpdate(grid.Cursor{X: 2, Y: 6},
		"> refactor the client dial path",
		"",
		"  Reading internal/client/client.go...",
		"  Edited 3 files, 214 insertions",
		"",
		"  Should I run the tests? [y/n]",
		"> ",
	)
	// A marker past the card's crop width: invisible on the grid thumbnail,
	// visible full-screen — what the focus golden keys on.
	agent.Damage = append(agent.Damage, rowCellsAt(60, 0, "EDGE-MARKER")...)

	tests := screenUpdate(grid.Cursor{X: 2, Y: 6},
		"$ go test ./...",
		"ok   internal/emu     0.41s",
		"ok   internal/wire    0.09s",
		"FAIL internal/tui     0.88s",
		"宽字符 wide cells stay aligned",
		"",
		"$ ",
	)

	editor := screenUpdate(grid.Cursor{X: 0, Y: 4},
		"# Release notes",
		"",
		"- Pane grid dashboard with live thumbnails",
		"- Focus follows the Client viewport",
		"~",
	)

	return &fakeConn{
		panes: []herd.Pane{
			{ID: "%1", Title: "claude · api refactor"},
			{ID: "%2", Title: "claude · test suite"},
			{ID: "%3", Title: "vim · release notes"},
		},
		streams: map[string]*fakeStream{
			"%1": {park: park, updates: []client.PaneUpdate{resize(80, 24), agent}},
			"%2": {park: park, updates: []client.PaneUpdate{resize(80, 24), tests}},
			"%3": {park: park, updates: []client.PaneUpdate{resize(80, 24), editor}},
		},
		sink: &fakeSink{},
	}
}

// goldenModel runs the dashboard fixture under the real runtime until every
// Pane's thumbnail is live.
func goldenModel(t *testing.T, conn *fakeConn) *teatest.TestModel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tm := teatest.NewTestModel(t, New(ctx, single(conn)), teatest.WithInitialTermSize(120, 36))
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := ansi.Strip(string(bts))
		return strings.Contains(s, "api refactor") &&
			strings.Contains(s, "FAIL internal/tui") &&
			strings.Contains(s, "Release notes")
	}, teatest.WithDuration(testutil.WaitTimeout))
	return tm
}

func TestGolden_GridView(t *testing.T) {
	tm := goldenModel(t, goldenConn(t))

	tm.Type("q")
	fm, ok := tm.FinalModel(t, teatest.WithFinalTimeout(testutil.WaitTimeout)).(Model)
	if !ok {
		t.Fatal("final model is not the dashboard Model")
	}
	golden.RequireEqual(t, goldenFrame(viewContent(fm)))
}

func TestGolden_FocusView(t *testing.T) {
	conn := goldenConn(t)
	tm := goldenModel(t, conn)

	// Focus the first Pane; the edge marker only fits the full-screen view,
	// so its appearance is the focus transition completing.
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		return strings.Contains(ansi.Strip(string(bts)), "EDGE-MARKER")
	}, teatest.WithDuration(testutil.WaitTimeout))

	// 'q' would be forwarded to the Pane in passthrough; end the program
	// directly instead.
	tm.Quit() //nolint:errcheck // always returns nil
	fm, ok := tm.FinalModel(t, teatest.WithFinalTimeout(testutil.WaitTimeout)).(Model)
	if !ok {
		t.Fatal("final model is not the dashboard Model")
	}
	golden.RequireEqual(t, goldenFrame(viewContent(fm)))

	// Focus also resized the Pane to the viewport, over the wire.
	waitResizes(t, conn.sink, resizeCall{pane: "%1", size: grid.Size{W: 120, H: 36}})
}
