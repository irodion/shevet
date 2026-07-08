package server

import (
	"time"

	"github.com/irodion/shevet/internal/emu"
	"github.com/irodion/shevet/internal/grid"
)

// flushInterval is the damage coalescing window (spec §5.1): output marks
// the pipeline dirty and arms a one-shot timer; the flush diffs the grid at
// most this often. Idle panes have no timer running and cost zero wakeups.
const flushInterval = 16 * time.Millisecond

// subscriberBuffer is each subscriber's update queue. A subscriber that
// falls this far behind stops receiving increments and gets one full-grid
// resync when it catches up — bounded memory regardless of Client speed.
const subscriberBuffer = 64

// renderUpdate is one message bound for a Pane's render streams. Fields are
// orthogonal: a resize and damage may travel together (resize first on the
// wire). Pane exit is not an update — it is the subscriber's exited flag,
// so a full queue can never drop it.
type renderUpdate struct {
	// resized, when non-nil, is the pane's new size. Receivers reset
	// their grid to default cells.
	resized *grid.Size

	// damage is the coalesced cell damage; cursor is the cursor state at
	// flush time and is meaningful whenever damage is non-nil, including
	// cursor-only batches (len(damage) == 0).
	damage []grid.CellPatch
	cursor grid.Cursor
}

// subscriber is one WatchPane stream's view of a pipeline. Updates arrive
// on ch, which the pipeline closes when the pane exits, the subscriber is
// removed, or the Server shuts down.
type subscriber struct {
	ch chan renderUpdate

	// exited is set before ch closes when the Pane left the Herd (rather
	// than the Server shutting down); the channel close ordering makes it
	// safe to read after ch is drained. It is a flag, not a queued update,
	// so the protocol's terminal PaneExited survives any backlog.
	exited bool

	// lagged is pipeline-goroutine state: the subscriber's queue overflowed
	// and it owes a full resync instead of increments.
	lagged bool
}

// pipeline is the per-Pane render pipeline: it owns the Pane's emulator
// (one goroutine end-to-end, ADR-0006), coalesces damage on the 16ms
// window, and fans flushes out to subscribers.
//
// All interaction goes through the ops channel into the run loop; the only
// concurrent-safe methods are output, resize, subscribe, unsubscribe, and
// close, each of which posts an op.
type pipeline struct {
	ops  chan any
	done chan struct{} // closed when the run loop ends
}

// Pipeline op types. Each is handled synchronously inside run.
type opOutput struct{ data []byte }
type opResize struct{ w, h int }
type opSubscribe struct{ sub *subscriber }
type opUnsubscribe struct{ sub *subscriber }
type opClose struct{ exited bool }

// newPipeline starts a pipeline for a pane with a w×h grid.
func newPipeline(w, h int) *pipeline {
	p := &pipeline{
		ops:  make(chan any, 64),
		done: make(chan struct{}),
	}
	go p.run(w, h)
	return p
}

// post delivers an op unless the pipeline has already ended (posting to a
// closed pane is a no-op, not an error: topology changes race RPCs).
func (p *pipeline) post(op any) {
	select {
	case p.ops <- op:
	case <-p.done:
	}
}

// output feeds pane output bytes into the emulator.
func (p *pipeline) output(data []byte) { p.post(opOutput{data: data}) }

// resize resizes the pane's grid; subscribers get a resize update and the
// following flushes re-establish content.
func (p *pipeline) resize(w, h int) { p.post(opResize{w: w, h: h}) }

// subscribe attaches a new render stream. Its channel first receives the
// full current state (resize + snapshot damage), then live updates.
func (p *pipeline) subscribe() *subscriber {
	sub := &subscriber{ch: make(chan renderUpdate, subscriberBuffer)}
	p.post(opSubscribe{sub: sub})
	return sub
}

// unsubscribe detaches a subscriber and closes its channel.
func (p *pipeline) unsubscribe(sub *subscriber) { p.post(opUnsubscribe{sub: sub}) }

// close ends the pipeline. With exited true subscribers are told the Pane
// left the Herd; false (Server shutdown) just closes their channels.
func (p *pipeline) close(exited bool) {
	p.post(opClose{exited: exited})
	<-p.done
}

// run is the pipeline goroutine: the single owner of the emulator, the
// shadow grid, and the subscriber set.
func (p *pipeline) run(w, h int) {
	defer close(p.done)

	term := emu.New(w, h)
	shadow := grid.New(w, h) // last flushed state
	scratch := grid.New(w, h)
	cursor := grid.Cursor{}
	subs := make(map[*subscriber]struct{})

	// The coalescing timer is armed on the first dirtying op after a
	// flush — and only while someone is watching: an unwatched pane costs
	// no snapshots, no diffs, and no wakeups. Its accumulated state is
	// folded in by the flush a subscriber attach performs.
	dirty := false
	timer := time.NewTimer(flushInterval)
	timer.Stop()
	armed := false
	arm := func() {
		if !armed && len(subs) > 0 {
			timer.Reset(flushInterval)
			armed = true
		}
	}

	// deliver sends an update, downgrading an overflowing subscriber to
	// lagged: it will get a full resync once its queue has room again.
	deliver := func(sub *subscriber, u renderUpdate) {
		select {
		case sub.ch <- u:
		default:
			sub.lagged = true
		}
	}

	// syncUpdate builds the full-state update a fresh or lagged
	// subscriber needs: reset to current size, then every non-default cell.
	syncUpdate := func() renderUpdate {
		sw, sh := shadow.Size()
		return renderUpdate{resized: &grid.Size{W: sw, H: sh}, damage: shadow.Snapshot(), cursor: cursor}
	}

	flush := func() {
		var u renderUpdate
		changed := false
		if dirty {
			dirty = false
			term.Snapshot(scratch)
			damage := grid.Diff(shadow, scratch)
			cur := term.Cursor()
			if len(damage) > 0 || cur != cursor {
				shadow, scratch = scratch, shadow
				cursor = cur
				if damage == nil {
					damage = []grid.CellPatch{} // cursor-only batch, still damage-shaped
				}
				u = renderUpdate{damage: damage, cursor: cursor}
				changed = true
			}
		}

		stillLagged := false
		for sub := range subs {
			if sub.lagged {
				// Catch a lagged subscriber up with one full resync
				// instead of a queue of increments.
				select {
				case sub.ch <- syncUpdate():
					sub.lagged = false
				default:
					stillLagged = true
				}
				continue
			}
			if changed {
				deliver(sub, u)
				stillLagged = stillLagged || sub.lagged
			}
		}
		// A lagged subscriber must not depend on the pane producing more
		// output: keep the flush cadence alive until its resync lands,
		// even on an otherwise idle pane.
		if stillLagged {
			arm()
		}
	}

	for {
		var op any
		select {
		case op = <-p.ops:
		case <-timer.C:
			armed = false
			flush()
			continue
		}

		switch op := op.(type) {
		case opOutput:
			term.Write(op.data) //nolint:errcheck // the emulator consumes everything
			dirty = true
			arm()

		case opResize:
			if w, h := term.Size(); w == op.w && h == op.h {
				continue
			}
			term.Resize(op.w, op.h)
			// The shadow resets so the next flush re-establishes all
			// content relative to a default grid — exactly what
			// subscribers hold after applying the resize.
			shadow = grid.New(op.w, op.h)
			scratch = grid.New(op.w, op.h)
			u := renderUpdate{resized: &grid.Size{W: op.w, H: op.h}}
			for sub := range subs {
				deliver(sub, u)
			}
			dirty = true
			arm()

		case opSubscribe:
			// Fold any pending damage into the shadow before adding the
			// subscriber: its first message must be the sync's resize
			// (the documented stream contract), never a stray damage
			// batch from this flush. The fresh queue always has room for
			// the sync.
			flush()
			subs[op.sub] = struct{}{}
			op.sub.ch <- syncUpdate()

		case opUnsubscribe:
			if _, ok := subs[op.sub]; ok {
				delete(subs, op.sub)
				close(op.sub.ch)
			}

		case opClose:
			for sub := range subs {
				// The flag, set before the close, cannot be dropped the
				// way a queued update to a full channel would be.
				sub.exited = op.exited
				close(sub.ch)
			}
			return
		}
	}
}
