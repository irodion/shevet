package harness

import (
	"runtime"
	"testing"
	"time"
)

// These are the acceptance tests of issue #54 (ADR-0008): ingest backpressure
// keeps a flooding Pane from exhausting the Server, and never lets one Pane's
// flood stall the others. The Server runs in-process here (harness.StartServer),
// so the flood flows through the real tmux control client into the same heap
// the test measures.

// floodCommand is a shell one-liner that writes to its pane as fast as tmux
// will carry it. seq in a tight loop is the #48 repro's generator.
const floodCommand = "while :; do seq 1 100000; done"

// TestFlood_BoundedServerMemory is the #48 repro: a full-speed flood into a
// watched pane, with no gRPC Client consuming it, must hold the Server heap
// bounded and stable instead of growing ~50MB/s toward multi-GB RSS.
func TestFlood_BoundedServerMemory(t *testing.T) {
	// Deliberately not parallel: it reads process-wide heap statistics, which
	// only mean something while no other test is allocating concurrently.
	h := Start(t)
	pane := h.Tmux.NewWindow(t, "flood", floodCommand)
	waitForPaneInHerd(t, h.Client, pane) // the watcher is now feeding the flood

	liveHeap := func() uint64 {
		runtime.GC() // drop transient flood garbage; keep only retained bytes
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	// Sample across a multi-second window: pre-fix the retained heap climbs
	// monotonically (the unbounded event queue); with flow control it plateaus.
	time.Sleep(2 * time.Second)
	first := liveHeap()
	time.Sleep(3 * time.Second)
	second := liveHeap()

	const (
		ceiling = 256 << 20 // 256 MB — pre-fix this window alone adds >100MB and keeps going
		drift   = 64 << 20  // tolerated growth between samples; pre-fix it is ~150MB
	)
	if first > ceiling || second > ceiling {
		t.Fatalf("heap not bounded under flood: %dMB then %dMB (ceiling %dMB)",
			first>>20, second>>20, ceiling>>20)
	}
	if second > first && second-first > drift {
		t.Errorf("heap still climbing under sustained flood: %dMB -> %dMB (drift cap %dMB)",
			first>>20, second>>20, drift>>20)
	}
}

// TestFlood_ResumesToAuthoritativeScreen covers the pause/resume/re-seed cycle:
// a pane floods, then settles on a known final line, and a watcher must
// converge to that authoritative screen — no loss, no stale flood content left
// behind.
func TestFlood_ResumesToAuthoritativeScreen(t *testing.T) {
	t.Parallel()
	h := Start(t)

	const marker = "FLOOD-SETTLED-MARKER"
	// A brief head start lets the watcher subscribe before the flood begins,
	// so the pause/resume/re-seed cycle runs while the pane is being watched
	// rather than being resolved by the initial seed. Losing the race only
	// weakens the test (it falls back to seed-shows-marker), never breaks it.
	//
	// The flood is deliberately modest. Pausing a pane makes tmux stop draining
	// its pty, which throttles the producer itself (backpressure reaching all
	// the way back), so the marker — printed only after seq finishes — appears
	// sooner with a smaller flood. It is still far more than the pipeline's
	// queue holds, so it exercises the pause path; the pause mechanism proper is
	// proven deterministically by the pipeline and memory tests.
	pane := h.Tmux.NewWindow(t, "flood-then-settle",
		"sleep 0.5; seq 1 20000; printf '\\n"+marker+"\\n'; sleep 86400")

	view := watchPane(t, h.Client, pane)
	// Converging to the marker after a live flood means the pane was paused,
	// resumed, and re-seeded to its authoritative screen with no flood content
	// left behind.
	view.waitFor(marker)
}

// TestFlood_DoesNotStallOtherPanes covers the isolation guarantee: while one
// pane floods without pause, a second pane's output must still reach a watcher.
// Pre-fix, feeding the flood blocked the single watcher goroutine; now output
// submission never blocks.
func TestFlood_DoesNotStallOtherPanes(t *testing.T) {
	t.Parallel()
	h := Start(t)

	// A pane that never stops flooding.
	noisy := h.Tmux.NewWindow(t, "noisy", floodCommand)
	waitForPaneInHerd(t, h.Client, noisy)

	// A second, quiet pane: its single marker must arrive despite the flood.
	const marker = "QUIET-PANE-MARKER"
	quiet := h.Tmux.NewWindow(t, "quiet", "printf '"+marker+"\\n'; sleep 86400")

	view := watchPane(t, h.Client, quiet)
	view.waitFor(marker)
}
