package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/testutil"
)

// White-box tests for the render pipeline: coalescing, subscriber sync,
// lag recovery, and close semantics. The tmux-fed end-to-end behavior is
// covered by the harness watch tests.

// startPipeline returns a pipeline that is torn down with the test.
func startPipeline(t *testing.T, w, h int) *pipeline {
	t.Helper()
	p := newPipeline("%test", w, h)
	t.Cleanup(func() { p.close(false) })
	return p
}

// pipeView consumes a subscriber's updates into a persistent local grid, so
// consecutive waits keep composing the stream the way a real receiver does.
type pipeView struct {
	t   *testing.T
	sub *subscriber

	g       *grid.Grid
	updates []renderUpdate
}

func view(t *testing.T, sub *subscriber) *pipeView {
	return &pipeView{t: t, sub: sub, g: grid.New(0, 0)}
}

// waitFor applies updates until cond holds, returning those consumed by
// this wait.
func (v *pipeView) waitFor(cond func(g *grid.Grid) bool) []renderUpdate {
	v.t.Helper()
	deadline := time.After(testutil.WaitTimeout)
	var consumed []renderUpdate
	for !cond(v.g) {
		select {
		case u, ok := <-v.sub.ch:
			if !ok {
				v.t.Fatal("subscriber channel closed while waiting for content")
			}
			consumed = append(consumed, u)
			v.updates = append(v.updates, u)
			if u.resized != nil {
				v.g = grid.New(u.resized[0], u.resized[1])
			}
			for _, patch := range u.damage {
				v.g.Apply(patch)
			}
		case <-deadline:
			v.t.Fatalf("content never converged; %d updates, grid:\n%q", len(v.updates), v.g.RowText(0))
		}
	}
	return consumed
}

func TestPipeline_SubscriberGetsSyncThenIncrements(t *testing.T) {
	t.Parallel()
	p := startPipeline(t, 20, 4)
	p.output([]byte("before"))

	v := view(t, p.subscribe())
	v.waitFor(func(g *grid.Grid) bool { return g.RowText(0) == "before" })

	p.output([]byte(" after"))
	increments := v.waitFor(func(g *grid.Grid) bool { return g.RowText(0) == "before after" })

	if w, h := v.g.Size(); w != 20 || h != 4 {
		t.Errorf("grid size = %dx%d, want 20x4", w, h)
	}
	// The increment must be damage-sized, not screen-sized.
	for _, u := range increments {
		if len(u.damage) > 10 {
			t.Errorf("incremental update carries %d cells, want only the changed few", len(u.damage))
		}
	}
}

func TestPipeline_BurstCoalescesIntoFewFlushes(t *testing.T) {
	t.Parallel()
	p := startPipeline(t, 60, 4)
	v := view(t, p.subscribe())

	const writes = 50
	for i := 0; i < writes; i++ {
		p.output([]byte("x"))
	}

	updates := v.waitFor(func(g *grid.Grid) bool {
		return len(g.RowText(0)) == writes
	})
	if len(updates) >= writes/2 {
		t.Errorf("%d writes produced %d updates; the 16ms window is not coalescing", writes, len(updates))
	}
}

func TestPipeline_LaggedSubscriberGetsFullResync(t *testing.T) {
	t.Parallel()
	p := startPipeline(t, 40, 8)

	// Never drained while flushes accumulate: the queue must overflow into
	// the lagged state instead of growing without bound.
	v := view(t, p.subscribe())

	for i := 0; i < 2*subscriberBuffer; i++ {
		p.output([]byte(fmt.Sprintf("\x1b[1;1Hcount %04d", i)))
		time.Sleep(2 * flushInterval) // separate flushes, to overflow the queue
	}
	want := fmt.Sprintf("final %04d", 2*subscriberBuffer)
	p.output([]byte("\x1b[1;1H" + want))

	updates := v.waitFor(func(g *grid.Grid) bool { return g.RowText(0) == want })
	if len(updates) > subscriberBuffer+2 {
		t.Errorf("drained %d updates from a lagged subscriber, want a bounded queue plus one resync", len(updates))
	}
}

func TestPipeline_CloseExitedTellsSubscribers(t *testing.T) {
	t.Parallel()
	p := newPipeline("%test", 10, 2)
	sub := p.subscribe()
	<-sub.ch // initial sync

	p.close(true)

	var sawExited bool
	for u := range sub.ch {
		if u.exited {
			sawExited = true
		}
	}
	if !sawExited {
		t.Error("subscriber channel closed without an exited update")
	}
}

func TestPipeline_CloseShutdownJustClosesChannels(t *testing.T) {
	t.Parallel()
	p := newPipeline("%test", 10, 2)
	sub := p.subscribe()
	<-sub.ch

	p.close(false)

	for u := range sub.ch {
		if u.exited {
			t.Error("shutdown close reported the pane as exited")
		}
	}
}

func TestPipeline_ResizeResetsAndReestablishes(t *testing.T) {
	t.Parallel()
	p := startPipeline(t, 20, 4)
	p.output([]byte("wide content"))
	v := view(t, p.subscribe())
	v.waitFor(func(g *grid.Grid) bool { return g.RowText(0) == "wide content" })

	p.resize(10, 2)
	p.output([]byte("\x1b[2J\x1b[Hnarrow"))

	v.waitFor(func(g *grid.Grid) bool { return g.RowText(0) == "narrow" })
	if w, h := v.g.Size(); w != 10 || h != 2 {
		t.Errorf("grid size after resize = %dx%d, want 10x2", w, h)
	}
}
