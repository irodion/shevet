package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/irodion/shevet/internal/herd"
	"github.com/irodion/shevet/internal/inject"
	"github.com/irodion/shevet/internal/tmuxctl"
)

// TmuxOptions selects the tmux server and session the Server watches.
type TmuxOptions struct {
	// Socket is the tmux server's socket path; empty uses the user's
	// default tmux server.
	Socket string

	// Session is the tmux session whose panes form the Herd. Required.
	Session string
}

// watchedPane is the per-pane state: the render pipeline plus the watcher's
// bookkeeping. The pipe field is immutable after construction and safe to
// read concurrently; size is guarded (the watcher writes it on reconcile,
// the input service reads it to record pre-focus sizes); the remaining
// fields belong to the watcher goroutine.
type watchedPane struct {
	pipe *pipeline

	// sizeMu guards width and height, the pane's authoritative geometry
	// from the latest reconcile.
	sizeMu        sync.Mutex
	width, height int

	// seedBarrier drops output events that predate the pane's latest
	// capture-pane seed: their bytes are already on the captured screen.
	seedBarrier uint64

	// paused is set while the pane's output is paused in tmux (ADR-0008),
	// either because the Server paused a saturated pane or because tmux's
	// pause-after backstop fired. While paused, live output is dropped — a
	// scheduled resume re-seeds from the authoritative screen and supersedes
	// it. Watcher-goroutine state, no lock.
	paused bool
}

// size returns the pane's geometry as of the latest reconcile.
func (wp *watchedPane) size() (w, h int) {
	wp.sizeMu.Lock()
	defer wp.sizeMu.Unlock()
	return wp.width, wp.height
}

// setSize records the pane's geometry from a reconcile snapshot.
func (wp *watchedPane) setSize(w, h int) {
	wp.sizeMu.Lock()
	defer wp.sizeMu.Unlock()
	wp.width, wp.height = w, h
}

// paneHub is the single map of live panes, shared between the watcher
// (which owns pane lifecycles and all watchedPane fields) and RPC handlers
// (which only resolve a pane id to its pipeline).
type paneHub struct {
	mu    sync.RWMutex
	panes map[string]*watchedPane
}

func newPaneHub() *paneHub {
	return &paneHub{panes: make(map[string]*watchedPane)}
}

// pipe resolves a pane id to its render pipeline; nil when the pane is not
// in the Herd. This is the RPC handlers' entry point.
func (h *paneHub) pipe(paneID string) *pipeline {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if wp := h.panes[paneID]; wp != nil {
		return wp.pipe
	}
	return nil
}

func (h *paneHub) get(paneID string) *watchedPane {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.panes[paneID]
}

func (h *paneHub) put(paneID string, wp *watchedPane) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.panes[paneID] = wp
}

// sweep removes and returns every pane not in keep.
func (h *paneHub) sweep(keep map[string]bool) []*watchedPane {
	h.mu.Lock()
	defer h.mu.Unlock()
	var gone []*watchedPane
	for id, wp := range h.panes {
		if !keep[id] {
			delete(h.panes, id)
			gone = append(gone, wp)
		}
	}
	return gone
}

// drain removes and returns every pane.
func (h *paneHub) drain() []*watchedPane {
	return h.sweep(nil)
}

const (
	// pauseAfter is the tmux pause-after backstop (ADR-0008): the whole
	// control stream — not one pane — must fall this far behind before tmux
	// pauses on its own. The Server pauses individual saturated panes well
	// before this fires, so it only catches whole-stream overload.
	pauseAfter = 2 * time.Second

	// resumeDelay throttles a saturated pane's re-seed rate: after pausing,
	// the Server waits this long before it resumes and re-seeds, so a
	// sustained flood costs a bounded stream of re-seeds rather than a tight
	// pause/resume loop that would burn CPU.
	resumeDelay = 100 * time.Millisecond

	// resumeQueue is the resume-signal channel's buffer. At most one signal is
	// outstanding per paused pane (a pane stays paused until it resumes), so
	// this absorbs a whole Herd's worth without a timer ever having to wait;
	// beyond it, scheduleResume blocks rather than dropping, so a resume is
	// never lost (see scheduleResume).
	resumeQueue = 256
)

// watcher mirrors one tmux session into the Server: the Registry lists its
// panes and each pane's output feeds a render pipeline. It is the Server's
// half of the "tmux owns processes, Shevet owns interpretation" split
// (ARCHITECTURE.md §3.1).
type watcher struct {
	ctl      *tmuxctl.Client
	session  string
	registry *Registry
	hub      *paneHub
	log      *slog.Logger

	// resumeCh carries pane ids whose scheduled resume is due. A timer per
	// pause feeds it; run drains it and re-seeds the pane (ADR-0008).
	resumeCh chan string

	// done is closed when the watcher tears down, releasing any resume timers
	// still waiting to hand their pane to run.
	done chan struct{}
}

// attachWatcher attaches to tmux in control mode and performs the initial
// pane enumeration. Both a failed attach (no server, no session) and a
// failed first reconcile are reported here, synchronously — and because the
// first reconcile completes before this returns, a Server that serves RPCs
// only afterwards presents a populated Herd from its very first response.
func attachWatcher(ctx context.Context, opts TmuxOptions, registry *Registry, hub *paneHub, log *slog.Logger) (*watcher, error) {
	ctl, err := tmuxctl.Attach(ctx, tmuxctl.Options{Socket: opts.Socket, Session: opts.Session, PauseAfter: pauseAfter})
	if err != nil {
		return nil, err
	}
	w := &watcher{
		ctl:      ctl,
		session:  opts.Session,
		registry: registry,
		hub:      hub,
		log:      log,
		resumeCh: make(chan string, resumeQueue),
		done:     make(chan struct{}),
	}
	if err := w.reconcile(ctx); err != nil {
		w.teardown()
		return nil, fmt.Errorf("initial pane enumeration: %w", err)
	}
	return w, nil
}

// commander exposes the control-mode client as the injection seam SendInput
// uses. It is the same client the watcher runs reconcile and seed on;
// tmuxctl.Client.Command is safe for concurrent use, so injecting alongside
// the watcher's own commands is well-defined.
func (w *watcher) commander() inject.Commander { return w.ctl }

// teardown empties the Herd, closes every pipeline, and detaches from tmux.
func (w *watcher) teardown() {
	close(w.done) // release resume timers still waiting on run
	w.registry.Replace(nil)
	for _, wp := range w.hub.drain() {
		wp.pipe.close(false)
	}
	w.ctl.Close() //nolint:errcheck // teardown; the stream is already done
}

// run consumes the control-mode event stream until it ends. On return the
// Herd is empty and every pipeline is closed; the error says why tmux went
// away (nil when ctx ended: that is Server shutdown, not failure).
func (w *watcher) run(ctx context.Context) error {
	defer w.teardown()

	for {
		select {
		case <-ctx.Done():
			return nil
		case paneID := <-w.resumeCh:
			w.resumePane(ctx, paneID)
		case ev, ok := <-w.ctl.Events():
			if !ok {
				return errors.New("tmux control stream ended")
			}

			// Absorb everything already queued before acting: tmux emits
			// notification bursts (a split raises several, a resize drag
			// a continuous stream), and one reconcile serves them all.
			// Handling output ahead of a pending reconcile is safe: a
			// pane the reconcile will create gets that output via its
			// capture-pane seed instead.
			topology, err := w.apply(ctx, ev)
		drain:
			for err == nil {
				select {
				case ev, ok = <-w.ctl.Events():
					if !ok {
						return errors.New("tmux control stream ended")
					}
					var t bool
					t, err = w.apply(ctx, ev)
					topology = topology || t
				default:
					break drain
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if topology {
				if err := w.reconcile(ctx); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("reconcile: %w", err)
				}
			}
		}
	}
}

// apply handles one notification, reporting whether it calls for a
// reconcile. An error means the control client is over.
func (w *watcher) apply(ctx context.Context, ev tmuxctl.Event) (topology bool, err error) {
	switch ev := ev.(type) {
	case tmuxctl.OutputEvent:
		wp := w.hub.get(ev.PaneID)
		if wp == nil || ev.Seq < wp.seedBarrier || wp.paused {
			// Unknown pane, pre-seed bytes, or a paused pane whose output a
			// pending re-seed will supersede: drop it.
			return false, nil
		}
		if !wp.pipe.output(ev.Data) {
			// The emulator is behind: pause the pane in tmux so its output
			// stops accumulating on the Server heap, then schedule a resume
			// that re-seeds it (ADR-0008).
			w.pausePane(ctx, ev.PaneID, wp)
		}
	case tmuxctl.PauseEvent:
		// tmux paused a pane: either the echo of the Server's own pause (the
		// pane is already marked) or the pause-after backstop firing on its
		// own. Adopt the latter and make sure a resume is scheduled.
		if wp := w.hub.get(ev.PaneID); wp != nil && !wp.paused {
			wp.paused = true
			w.scheduleResume(ev.PaneID)
		}
	case tmuxctl.ContinueEvent:
		// Informational: the Server drives every resume itself.
	case tmuxctl.TopologyEvent:
		return true, nil
	case tmuxctl.ExitEvent:
		return false, fmt.Errorf("tmux closed the control client %s", strings.TrimSpace(ev.Reason))
	}
	return false, nil
}

// pausePane pauses a saturated pane's output in tmux and schedules its resume.
// It marks the pane first so the OutputEvents still queued behind this one are
// dropped instead of racing the pause.
//
// Flow-control commands are never fatal to the watcher: a pane can vanish
// between the output event and the command (a normal topology race), and one
// pane's failed pause must not tear down the whole session. A pause that fails
// still schedules a resume — the re-seed recovers the pane whether or not the
// pause took effect — so the pane can never wedge paused with nothing to
// unpause it.
func (w *watcher) pausePane(ctx context.Context, paneID string, wp *watchedPane) {
	wp.paused = true
	if _, err := w.ctl.Command(ctx, "refresh-client", "-A", paneID+":pause"); err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		w.log.Warn("pause saturated pane failed", "pane", paneID, "error", err)
	}
	w.scheduleResume(paneID)
}

// scheduleResume asks run to resume paneID after resumeDelay. The send never
// drops: resumeCh buffers a whole Herd's worth of pending resumes, and if it
// somehow fills the timer waits for run to drain one rather than lose the only
// signal that would unpause the pane. It aborts on teardown so the timer
// goroutine can never outlive the watcher.
func (w *watcher) scheduleResume(paneID string) {
	time.AfterFunc(resumeDelay, func() {
		select {
		case w.resumeCh <- paneID:
		case <-w.done:
		}
	})
}

// resumePane re-seeds a paused pane from its authoritative screen, then resumes
// its output. The order is the canonical tmux control-mode recovery and is
// load-bearing: tmux never replays a paused pane's output on continue — it
// expects a capture-pane resync — so seeding while still paused captures a
// frozen, authoritative screen (verified against tmux: a burst produced while
// paused yields no %output/%extended-output before or after continue). seed's
// barrier, the capture's stream position, drops any pre-pause output still in
// flight; and because continue is issued last, the only output that reaches the
// pipeline afterward is produced after it (Seq past the barrier), composed onto
// the seed with no duplication — the same degraded reconstruction as a reconnect
// (ARCHITECTURE.md §5.3). Continuing last also keeps the producer throttled
// until the seed has landed.
//
// Like pausePane, failures here are recovered rather than fatal: a re-seed or
// continue that fails reschedules another resume, so a live pane never stays
// stuck paused.
func (w *watcher) resumePane(ctx context.Context, paneID string) {
	wp := w.hub.get(paneID)
	if wp == nil || !wp.paused {
		return // pane gone, or already resumed
	}
	if err := w.seed(ctx, paneID, wp); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("re-seed on resume failed; retrying", "pane", paneID, "error", err)
		w.scheduleResume(paneID)
		return
	}
	wp.paused = false
	if _, err := w.ctl.Command(ctx, "refresh-client", "-A", paneID+":continue"); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.log.Warn("continue pane failed; retrying", "pane", paneID, "error", err)
		wp.paused = true
		w.scheduleResume(paneID)
	}
}

// paneInfo is one line of the reconcile snapshot.
type paneInfo struct {
	id            string
	width, height int
	title         string
}

// paneTitleFormat names a Pane for dashboards: the pane title when the
// inner application set one (agents commonly title their terminal), else
// the window name — tmux defaults an untouched pane title to the hostname
// (#{host}), which says nothing about the Agent, while the window name is
// what Spawn (and users) name the window.
const paneTitleFormat = "#{?#{!=:#{pane_title},#{host}},#{pane_title},#{window_name}}"

// reconcile realigns the Registry and the pane set with an authoritative
// list-panes snapshot. One code path serves initial sync and every topology
// change: create what's new, resize-and-reseed what changed, close what's
// gone. Reconciling is idempotent, so over-triggering on uninteresting
// notifications is safe.
func (w *watcher) reconcile(ctx context.Context) error {
	lines, err := w.ctl.Command(ctx, "list-panes", "-s", "-t", "="+w.session,
		"-F", "#{pane_id}\t#{pane_width}\t#{pane_height}\t"+paneTitleFormat)
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(lines))
	panes := make([]herd.Pane, 0, len(lines))
	for _, line := range lines {
		info, err := parsePaneLine(line)
		if err != nil {
			return err
		}
		seen[info.id] = true
		panes = append(panes, herd.Pane{ID: info.id, Title: info.title})

		wp := w.hub.get(info.id)
		switch {
		case wp == nil:
			// New pane: pipeline plus a seed of its current screen —
			// which is empty for panes born after the Server attached,
			// making them full-fidelity from their first byte.
			wp = &watchedPane{pipe: newPipeline(info.width, info.height)}
			wp.setSize(info.width, info.height)
			w.hub.put(info.id, wp)
			if err := w.seed(ctx, info.id, wp); err != nil {
				return err
			}
		default:
			if pw, ph := wp.size(); pw != info.width || ph != info.height {
				// Resized pane: tmux reflows content in ways an emulator
				// resize does not reproduce, so resize and re-seed from
				// the authoritative screen.
				wp.setSize(info.width, info.height)
				wp.pipe.resize(info.width, info.height)
				if err := w.seed(ctx, info.id, wp); err != nil {
					return err
				}
			}
		}
	}

	// Panes that disappeared from the snapshot have exited.
	for _, wp := range w.hub.sweep(seen) {
		wp.pipe.close(true)
	}

	// Keep tmux's own enumeration order: it is stable (window index, then
	// pane index) and matches what the developer sees in tmux itself.
	w.registry.Replace(panes)
	return nil
}

// seed initializes (or re-baselines) a pipeline from the pane's current
// screen: rendered rows with their SGR styling (capture-pane -e) and the
// cursor position, read as one atomic snapshot (see CommandsSeq). Output
// events that predate the snapshot are dropped via the seed barrier — their
// bytes are already on the captured screen — so the seed and the live stream
// compose without duplication or loss.
//
// A seed is the documented degraded reconstruction (ARCHITECTURE.md §5.3):
// exact visible content and styling, but no scrollback and no in-flight
// escape state. Panes created after the Server attached skip the loss: they
// are seeded from an empty screen.
func (w *watcher) seed(ctx context.Context, paneID string, wp *watchedPane) error {
	// Capture the screen and read the cursor as one atomic control-mode
	// sequence, so both — and the barrier — pin to a single stream position.
	// Two separate commands let output land between them, making the rows,
	// cursor, and barrier disagree and the replay run from the wrong origin
	// (issue #53). The barrier is the snapshot's position (the first reply):
	// output already on the captured screen has a smaller Seq and is dropped.
	replies, seq, err := w.ctl.CommandsSeq(ctx,
		[]string{"capture-pane", "-p", "-e", "-t", paneID},
		[]string{"display-message", "-p", "-t", paneID, "#{cursor_x}\t#{cursor_y}"},
	)
	if err != nil {
		return err
	}
	// CommandsSeq returns one reply body per command on success.
	rows, cursor := replies[0], replies[1]
	cx, cy := 0, 0
	if len(cursor) == 1 {
		fmt.Sscanf(cursor[0], "%d\t%d", &cx, &cy) //nolint:errcheck // home cursor on mismatch beats failing the pane
	}

	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[2J\x1b[H") // pristine state even on re-seed
	for i, row := range rows {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(row)
	}
	// Neutralize styling bleed from the captured rows, then place the cursor.
	fmt.Fprintf(&b, "\x1b[0m\x1b[%d;%dH", cy+1, cx+1)

	wp.seedBarrier = seq
	wp.pipe.deliverSeed([]byte(b.String()))
	return nil
}

// maxPaneDim bounds accepted pane dimensions: far beyond any real terminal,
// tight enough that a garbled geometry cannot become a giant allocation.
const maxPaneDim = 16384

// parsePaneLine parses one reconcile line: id, width, height, title,
// tab-separated (titles may contain anything but tabs). Geometry is
// validated here, at the boundary where tmux data enters the Server.
func parsePaneLine(line string) (paneInfo, error) {
	parts := strings.SplitN(line, "\t", 4)
	if len(parts) != 4 {
		return paneInfo{}, fmt.Errorf("malformed list-panes line %q", line)
	}
	var info paneInfo
	info.id = parts[0]
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2], "%d %d", &info.width, &info.height); err != nil {
		return paneInfo{}, fmt.Errorf("malformed pane geometry in %q: %w", line, err)
	}
	if info.width <= 0 || info.height <= 0 || info.width > maxPaneDim || info.height > maxPaneDim {
		return paneInfo{}, fmt.Errorf("implausible pane geometry %dx%d in %q", info.width, info.height, line)
	}
	info.title = parts[3]
	return info, nil
}
