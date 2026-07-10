package server

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/emu"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/testutil"
)

// White-box tests for the render pipeline: coalescing, subscriber sync,
// lag recovery, and close semantics. The tmux-fed end-to-end behavior is
// covered by the harness watch tests.

// startPipeline returns a pipeline that is torn down with the test.
func startPipeline(t *testing.T, w, h int) *pipeline {
	t.Helper()
	p := newPipeline(w, h)
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
				v.g = grid.New(u.resized.W, u.resized.H)
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

// TestPipeline_FirstUpdateIsAlwaysTheResize pins the stream contract: a
// subscriber's first message is the initial resize, even when the pane
// accumulated unwatched output that the attach itself flushes.
func TestPipeline_FirstUpdateIsAlwaysTheResize(t *testing.T) {
	t.Parallel()
	p := startPipeline(t, 20, 4)

	// Dirty, unflushed state: with no subscribers the coalescer never ran.
	p.output([]byte("dirty before anyone watches"))

	sub := p.subscribe()
	select {
	case u := <-sub.ch:
		if u.resized == nil {
			t.Fatalf("first update = %+v, want the initial resize", u)
		}
	case <-time.After(testutil.WaitTimeout):
		t.Fatal("no update arrived for a fresh subscriber")
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

// TestPipeline_LaggedSubscriberResyncsOnIdlePane pins the recovery
// guarantee: a subscriber that overflowed during a burst converges to the
// pane's final state even when the pane never produces another byte — the
// pipeline keeps retrying the resync at the flush cadence, not only on the
// next write.
func TestPipeline_LaggedSubscriberResyncsOnIdlePane(t *testing.T) {
	t.Parallel()
	p := startPipeline(t, 40, 8)
	v := view(t, p.subscribe())

	// Overflow the undrained queue, then go idle: no further writes.
	final := ""
	for i := 0; i < 2*subscriberBuffer; i++ {
		final = fmt.Sprintf("count %04d", i)
		p.output([]byte("\x1b[1;1H" + final))
		time.Sleep(2 * flushInterval) // separate flushes, to overflow the queue
	}

	v.waitFor(func(g *grid.Grid) bool { return g.RowText(0) == final })
}

// gatedEmu is a test emulator whose Write blocks until the test releases a
// gate, so a test can stall the pipeline's single goroutine and observe how
// output submission and coalescing behave while the emulator is behind. It
// signals entered on each Write entry (before blocking), which lets a test
// wait until the pipeline is provably stalled with an empty queue.
type gatedEmu struct {
	w, h    int
	gate    chan struct{} // Write blocks receiving from this until it is closed
	entered chan struct{} // buffered: one value per Write entry
	writes  atomic.Int32
}

func (e *gatedEmu) Write(p []byte) (int, error) {
	e.entered <- struct{}{} // announce entry before blocking (buffered: never stalls)
	<-e.gate                // released once the gate is closed
	e.writes.Add(1)
	return len(p), nil
}
func (e *gatedEmu) Resize(w, h int)         { e.w, e.h = w, h }
func (e *gatedEmu) Size() (int, int)        { return e.w, e.h }
func (e *gatedEmu) Snapshot(dst *grid.Grid) { dst.Resize(e.w, e.h) }
func (e *gatedEmu) Cursor() grid.Cursor     { return grid.Cursor{} }

// gatedPipeline starts a pipeline backed by a gatedEmu the test controls. The
// emulator is built in the test goroutine and only its fields are touched from
// the run goroutine, so the fake pointer is never assigned across goroutines.
func gatedPipeline(t *testing.T, w, h int) (*pipeline, *gatedEmu, chan struct{}) {
	t.Helper()
	fake := &gatedEmu{gate: make(chan struct{}), entered: make(chan struct{}, 4*opsQueue)}
	p := newPipelineWith(w, h, func(w, h int) emu.Emulator {
		fake.w, fake.h = w, h
		return fake
	})
	return p, fake, fake.gate
}

// TestPipeline_OutputReportsSaturation pins the backpressure signal (ADR-0008):
// while the emulator is behind, output() fills the bounded queue and then
// reports not-accepted instead of blocking the caller — the watcher's cue to
// pause the pane in tmux.
func TestPipeline_OutputReportsSaturation(t *testing.T) {
	t.Parallel()
	p, fake, gate := gatedPipeline(t, 20, 4)
	t.Cleanup(func() { close(gate); p.close(false) })

	// Stall the run goroutine inside Write with an empty queue, so what fills
	// next is purely the bounded channel.
	if !p.output([]byte("x")) {
		t.Fatal("first output() was rejected before the pipeline could stall")
	}
	<-fake.entered

	// The bounded queue fills, then output() must report saturation rather
	// than block — within one queue's worth of submissions.
	saturated := false
	for i := 0; i < opsQueue+1; i++ {
		if !p.output([]byte("x")) {
			saturated = true
			break
		}
	}
	if !saturated {
		t.Fatalf("output() never reported saturation after filling a %d-slot queue", opsQueue)
	}
}

// TestPipeline_CoalescesQueuedOutputIntoOneWrite pins AC #4: output queued
// while the emulator is behind drains as a single coalesced write when it
// catches up, not one write per line.
func TestPipeline_CoalescesQueuedOutputIntoOneWrite(t *testing.T) {
	t.Parallel()
	p, fake, gate := gatedPipeline(t, 20, 4)
	t.Cleanup(func() { p.close(false) })

	// Stall the pipeline inside the first write with an empty queue.
	p.output([]byte("first"))
	<-fake.entered

	// Queue a burst behind the stalled write; it all sits in the channel.
	const burst = 40
	queued := 0
	for i := 0; i < burst; i++ {
		if p.output([]byte("y")) {
			queued++
		}
	}

	close(gate) // let the emulator catch up: first write completes, burst drains
	<-fake.entered
	testutil.Eventually(t, "queued burst to drain", func() (bool, string) {
		return fake.writes.Load() >= 2, fmt.Sprintf("writes=%d", fake.writes.Load())
	})

	// The whole queued burst became a single coalesced write (plus the one
	// in-flight first write): far fewer writes than outputs.
	if got := fake.writes.Load(); int(got) > 2 {
		t.Errorf("%d queued outputs produced %d emulator writes past the first; coalescing is not happening", queued, got-1)
	}
}

func TestPipeline_CloseExitedTellsSubscribers(t *testing.T) {
	t.Parallel()
	p := newPipeline(10, 2)
	sub := p.subscribe()

	// Leave the queue undrained: the exited signal is a flag set before
	// the channel close, so no backlog can drop it.
	p.close(true)

	for range sub.ch {
	}
	if !sub.exited {
		t.Error("subscriber channel closed without the exited flag")
	}
}

func TestPipeline_CloseShutdownJustClosesChannels(t *testing.T) {
	t.Parallel()
	p := newPipeline(10, 2)
	sub := p.subscribe()
	<-sub.ch

	p.close(false)

	for range sub.ch {
	}
	if sub.exited {
		t.Error("shutdown close reported the pane as exited")
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
