package harness

import (
	"context"
	"testing"

	"github.com/irodion/shevet/internal/testutil"
)

// waitForSize steps the stream until the grid reports w×h. The stream
// context enforces the deadline — a resize that never lands fails in Recv.
func (v *paneView) waitForSize(w, h int) {
	v.t.Helper()
	for {
		gw, gh := v.view.Grid.Size()
		if gw == w && gh == h {
			return
		}
		v.step()
	}
}

// TestListPanes_TitleFallsBackToWindowName pins the dashboard-facing title
// contract: a pane whose application never set a title (tmux then reports
// the hostname) is named by its window instead — the name Spawn and users
// give it — so cards never read as a wall of hostnames.
func TestListPanes_TitleFallsBackToWindowName(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", "prompt ready > \nawait-line\n")
	waitForPaneInHerd(t, h.Client, pane)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	panes, err := h.Client.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	for _, p := range panes {
		if p.ID == pane {
			if p.Title != "agent-a" {
				t.Errorf("pane title = %q, want the window name %q", p.Title, "agent-a")
			}
			return
		}
	}
	t.Fatalf("pane %s not in the Herd: %v", pane, panes)
}

// TestResize_RequestResizesPaneAndReseeds is the Server half of the
// resize-on-focus contract (#11): a ResizeRequest on the Control Input
// stream resizes the Pane's tmux window, and the watcher relays the change
// to every render-stream subscriber as a PaneResized plus a re-seed of the
// authoritative screen — the same confirmation path as a tmux-initiated
// resize.
func TestResize_RequestResizesPaneAndReseeds(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", "prompt sized > \nawait-line\n")
	view := watchPane(t, h.Client, pane)
	view.waitFor("sized >")
	if w, hgt := view.view.Grid.Size(); w == 120 && hgt == 40 {
		t.Fatalf("pane already 120x40; the resize would be vacuous")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	in, err := h.Client.SendInput(ctx)
	if err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := in.SendResize(pane, 120, 40); err != nil {
		t.Fatalf("SendResize: %v", err)
	}

	// The render stream confirms: the new geometry, then the re-seeded
	// screen with the prompt intact.
	view.waitForSize(120, 40)
	view.waitFor("sized >")
}

// TestResize_RestoreReturnsThePreFocusSize is the unfocus half of the
// resize-on-focus contract against real tmux: the Server recorded the size
// the focusing resize displaced, and a bare RestoreSize returns the Pane to
// it — the Client never says what size that was.
func TestResize_RestoreReturnsThePreFocusSize(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", "prompt sized > \nawait-line\n")
	view := watchPane(t, h.Client, pane)
	view.waitFor("sized >")
	origW, origH := view.view.Grid.Size()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	in, err := h.Client.SendInput(ctx)
	if err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := in.SendResize(pane, 120, 40); err != nil {
		t.Fatalf("SendResize: %v", err)
	}
	view.waitForSize(120, 40)

	if err := in.SendRestore(pane); err != nil {
		t.Fatalf("SendRestore: %v", err)
	}
	view.waitForSize(origW, origH)
	view.waitFor("sized >") // the re-seeded screen is intact
}

// TestResize_StreamEndRestoresThePane covers the vanished-Client contract: a
// Control Input stream that ends with a Pane still resized restores it — the
// Pane must not stay stuck at a dead Client's viewport size.
func TestResize_StreamEndRestoresThePane(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", "prompt sized > \nawait-line\n")
	view := watchPane(t, h.Client, pane)
	view.waitFor("sized >")
	origW, origH := view.view.Grid.Size()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	in, err := h.Client.SendInput(ctx)
	if err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := in.SendResize(pane, 120, 40); err != nil {
		t.Fatalf("SendResize: %v", err)
	}
	view.waitForSize(120, 40)

	// The Client goes away without restoring.
	if _, err := in.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	view.waitForSize(origW, origH)
}
