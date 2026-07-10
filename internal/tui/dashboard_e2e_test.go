package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/irodion/shevet/internal/harness"
	"github.com/irodion/shevet/internal/testutil"
)

// These are acceptance e2e tests of issue #11: the dashboard runs under the
// real Bubble Tea runtime against a real Server, tmux sandbox, and scripted
// agents — the full render path, Client-side included.

// startDashboard runs the dashboard over the harness Server under the test
// runtime, on a w×h terminal.
func startDashboard(t *testing.T, h *harness.Harness, w, height int) *teatest.TestModel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	servers, err := NewServers([]Server{{Alias: testAlias, Conn: FromClient(h.Client)}})
	if err != nil {
		t.Fatalf("NewServers: %v", err)
	}
	return teatest.NewTestModel(t, New(ctx, servers), teatest.WithInitialTermSize(w, height))
}

// waitForFrame waits until the dashboard has rendered every given string
// (ANSI stripped: assertions are content, not colors).
func waitForFrame(t *testing.T, tm *teatest.TestModel, want ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(bts []byte) bool {
		s := ansi.Strip(string(bts))
		for _, w := range want {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(testutil.WaitTimeout))
}

// quitDashboard ends the program and waits for it to finish (in passthrough
// a 'q' would be forwarded to the Pane, so end it directly).
func quitDashboard(t *testing.T, tm *teatest.TestModel) {
	t.Helper()
	tm.Quit() //nolint:errcheck // always returns nil
	tm.WaitFinished(t, teatest.WithFinalTimeout(testutil.WaitTimeout))
}

// TestDashboard_FloodingPaneDoesNotStallOthers is the isolation criterion of
// issue #11, Client-side: while one Pane floods flat out, a quiet Pane's
// fresh output must still reach the rendered dashboard. The flood rides the
// Server's ingest backpressure (ADR-0008); this proves the whole path — tmux,
// Server, wire, dashboard — stays live for the other Panes.
func TestDashboard_FloodingPaneDoesNotStallOthers(t *testing.T) {
	t.Parallel()
	h := harness.Start(t)

	// A scripted agent that floods for the whole test (the sandbox teardown
	// kills it), and a quiet one that speaks only when poked.
	h.StartAgent(t, "noisy", "spam 2000000 flood")
	quiet := h.StartAgent(t, "quiet", strings.Join([]string{
		"prompt quiet ready > ",
		"await-line",
		"print QUIET-LIVE-MARKER",
		"prompt quiet > ",
		"await-line",
	}, "\n"))
	waitForPanes(t, h.Client, 3) // holder shell + noisy + quiet

	tm := startDashboard(t, h, 140, 40)
	// The quiet Pane's thumbnail is up while the flood rages...
	waitForFrame(t, tm, "quiet ready >")
	// ...and fresh output produced mid-flood still renders.
	h.Tmux.SendLine(t, quiet, "go")
	waitForFrame(t, tm, "QUIET-LIVE-MARKER")

	quitDashboard(t, tm)
}

// TestDashboard_FocusResizesPaneToViewportAndRestores is the resize-on-focus
// acceptance of issue #11 against real tmux: Enter resizes the Pane's window
// to the Client viewport; the leader restores the prior size.
func TestDashboard_FocusResizesPaneToViewportAndRestores(t *testing.T) {
	t.Parallel()
	h := harness.Start(t)

	pane := h.StartAgent(t, "agent", "prompt agent ready > \nawait-line\n")
	waitForPanes(t, h.Client, 2) // holder shell + agent

	size := func() string {
		return h.Tmux.Run(t, "display-message", "-p", "-t", pane, "#{pane_width}x#{pane_height}")
	}
	before := size()
	if before == "150x45" {
		t.Fatalf("pane already at the test viewport size %s; the resize would be vacuous", before)
	}

	tm := startDashboard(t, h, 150, 45)
	waitForFrame(t, tm, "agent ready >")

	// The holder shell is the first card; the agent is to its right.
	tm.Send(tea.KeyPressMsg{Code: tea.KeyRight})
	tm.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	testutil.Eventually(t, "the focused pane to match the viewport", func() (bool, string) {
		got := size()
		return got == "150x45", got
	})

	// Unfocus restores the pre-focus size.
	tm.Send(tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl})
	testutil.Eventually(t, "the pane to return to its prior size", func() (bool, string) {
		got := size()
		return got == before, got
	})

	quitDashboard(t, tm)
}
