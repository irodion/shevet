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

// opsQueue bounds the per-pane op channel. It doubles as the saturation
// threshold: when output() finds it full, the pane is producing faster than
// the emulator drains and the watcher pauses it (ADR-0008). Sized so a normal
// burst fits without a spurious pause, small enough to bound per-pane memory.
const opsQueue = 64

// maxRetainedBatch bounds the coalescing buffer a pane keeps once its output
// subsides. A wakeup that coalesces more than this keeps reusing its buffer, so
// steady bulk output (which can exceed it — one tmux %output decodes to well
// over 64 KiB) never re-allocates; once a later, smaller wakeup no longer needs
// the grown capacity, the buffer is released rather than pinned at the flood's
// high-water mark for the pane's lifetime (ADR-0008).
const maxRetainedBatch = 64 * 1024

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
func newPipeline(w, h int) *pipeline { return newPipelineWith(w, h, emu.New) }

// newPipelineWith is newPipeline with an injectable emulator constructor, the
// seam white-box tests use to drive coalescing and saturation deterministically
// with a controllable emulator.
func newPipelineWith(w, h int, newEmu func(w, h int) emu.Emulator) *pipeline {
	p := &pipeline{
		ops:  make(chan any, opsQueue),
		done: make(chan struct{}),
	}
	go p.run(w, h, newEmu)
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

// output feeds live pane output into the emulator without ever blocking the
// caller: it reports whether the pipeline accepted the bytes. A false return
// means the queue is full — the pane is producing faster than the emulator
// drains — and is the watcher's signal to pause the pane in tmux rather than
// let output accumulate on the Server heap (ADR-0008). A closed pane reports
// accepted: a dead pane must not be paused.
func (p *pipeline) output(data []byte) bool {
	select {
	case p.ops <- opOutput{data: data}:
		return true
	case <-p.done:
		return true
	default:
		return false
	}
}

// deliverSeed feeds a capture-pane seed into the emulator, blocking until the
// pipeline accepts it. Seeds are authoritative (they re-establish the whole
// screen) and low-volume, so they must land even while live output is being
// dropped for a saturated pane; the surrounding pause keeps the queue draining
// so this does not block for long.
func (p *pipeline) deliverSeed(data []byte) { p.post(opOutput{data: data}) }

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
func (p *pipeline) run(w, h int, newEmu func(w, h int) emu.Emulator) {
	defer close(p.done)

	term := newEmu(w, h)
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

	// outBatch coalesces consecutive output ops into a single emulator write
	// per wakeup (ADR-0008), so the parser's per-write overhead amortizes
	// across a burst instead of being paid line by line.
	var outBatch []byte
	writeBatch := func() {
		if len(outBatch) == 0 {
			return
		}
		term.Write(outBatch) //nolint:errcheck // the emulator consumes everything
		// Keep reusing the buffer while wakeups still fill it — a busy or
		// bulk-streaming pane never re-allocates. Release it only once an
		// oversized buffer outgrows what a wakeup needs (the flood that grew it
		// has passed), so a pane that falls quiet doesn't pin the flood's peak
		// for its lifetime (ADR-0008). This is subscriber-independent, so an
		// unwatched pane is bounded too.
		if cap(outBatch) > maxRetainedBatch && len(outBatch) <= maxRetainedBatch {
			outBatch = nil
		} else {
			outBatch = outBatch[:0]
		}
		dirty = true
		arm()
	}

	var burst []any
	for {
		var op any
		select {
		case op = <-p.ops:
		case <-timer.C:
			armed = false
			flush()
			continue
		}

		// Gather every op already queued for this wakeup in arrival order,
		// then apply it with consecutive output coalesced. Draining the
		// channel this way also keeps it short, so output()'s full-queue
		// check tracks the emulator falling behind rather than scheduling.
		burst = append(burst[:0], op)
	gather:
		for {
			select {
			case more := <-p.ops:
				burst = append(burst, more)
			default:
				break gather
			}
		}

		for _, op := range burst {
			if o, ok := op.(opOutput); ok {
				outBatch = append(outBatch, o.data...)
				continue
			}
			// A control op is ordered against the output around it: flush
			// the coalesced writes before applying it.
			writeBatch()
			switch op := op.(type) {
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
		// Flush output that trailed the last control op (or the whole burst
		// when it was output-only).
		writeBatch()
		// Release the burst's payload references before blocking for the next
		// wakeup: burst[:0] on the next gather would otherwise leave the tail
		// of a large flood pinning its opOutput payloads until overwritten.
		clear(burst)
	}
}
