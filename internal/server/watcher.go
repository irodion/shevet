package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/irodion/shevet/internal/herd"
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

// paneHub is the rendezvous between the watcher (which owns pane
// lifecycles) and RPC handlers (which look panes up concurrently).
type paneHub struct {
	mu        sync.RWMutex
	pipelines map[string]*pipeline
}

func newPaneHub() *paneHub {
	return &paneHub{pipelines: make(map[string]*pipeline)}
}

func (h *paneHub) get(paneID string) *pipeline {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pipelines[paneID]
}

func (h *paneHub) put(paneID string, p *pipeline) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pipelines[paneID] = p
}

func (h *paneHub) remove(paneID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pipelines, paneID)
}

// watchedPane is the watcher's bookkeeping for one live pane.
type watchedPane struct {
	pipe          *pipeline
	width, height int

	// seedBarrier drops output events that predate the pane's latest
	// capture-pane seed: their bytes are already on the captured screen.
	seedBarrier uint64
}

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

	// panes is single-goroutine state of the run loop; the hub carries the
	// concurrent view for RPC handlers.
	panes map[string]*watchedPane
}

// attachWatcher attaches to tmux in control mode and performs the initial
// pane enumeration. Both a failed attach (no server, no session) and a
// failed first reconcile are reported here, synchronously — and because the
// first reconcile completes before this returns, a Server that serves RPCs
// only afterwards presents a populated Herd from its very first response.
func attachWatcher(ctx context.Context, opts TmuxOptions, registry *Registry, hub *paneHub, log *slog.Logger) (*watcher, error) {
	ctl, err := tmuxctl.Attach(ctx, tmuxctl.Options{Socket: opts.Socket, Session: opts.Session})
	if err != nil {
		return nil, err
	}
	w := &watcher{
		ctl:      ctl,
		session:  opts.Session,
		registry: registry,
		hub:      hub,
		log:      log,
		panes:    make(map[string]*watchedPane),
	}
	if err := w.reconcile(ctx); err != nil {
		for id, wp := range w.panes {
			w.hub.remove(id)
			wp.pipe.close(false)
		}
		ctl.Close() //nolint:errcheck // already failing; process cleanup only
		return nil, fmt.Errorf("initial pane enumeration: %w", err)
	}
	return w, nil
}

// run consumes the control-mode event stream until it ends. On return the
// Herd is empty and every pipeline is closed; the error says why tmux went
// away (nil when ctx ended: that is Server shutdown, not failure).
func (w *watcher) run(ctx context.Context) error {
	defer func() {
		w.registry.Replace(nil)
		for id, wp := range w.panes {
			w.hub.remove(id)
			wp.pipe.close(false)
		}
		w.ctl.Close() //nolint:errcheck // teardown; the stream is already done
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ctl.Events():
			if !ok {
				return fmt.Errorf("tmux control stream ended")
			}
			if err := w.handle(ctx, ev); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// exitError signals tmux deliberately closed the control client.
type exitError struct{ reason string }

func (e exitError) Error() string {
	return strings.TrimSpace("tmux closed the control client " + e.reason)
}

func (w *watcher) handle(ctx context.Context, ev tmuxctl.Event) error {
	switch ev := ev.(type) {
	case tmuxctl.OutputEvent:
		if wp := w.panes[ev.PaneID]; wp != nil && ev.Seq >= wp.seedBarrier {
			wp.pipe.output(ev.Data)
		}
	case tmuxctl.TopologyEvent:
		if err := w.reconcile(ctx); err != nil {
			return fmt.Errorf("reconcile after %q: %w", ev.Notification, err)
		}
	case tmuxctl.ExitEvent:
		return exitError{reason: ev.Reason}
	}
	return nil
}

// paneInfo is one line of the reconcile snapshot.
type paneInfo struct {
	id            string
	width, height int
	title         string
}

// reconcile realigns the Registry and the pipeline set with an
// authoritative list-panes snapshot. One code path serves initial sync and
// every topology change: create what's new, resize-and-reseed what changed,
// close what's gone. Reconciling is idempotent, so over-triggering on
// uninteresting notifications is safe.
func (w *watcher) reconcile(ctx context.Context) error {
	lines, err := w.ctl.Command(ctx, "list-panes", "-s", "-t", "="+w.session,
		"-F", "#{pane_id}\t#{pane_width}\t#{pane_height}\t#{pane_title}")
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

		wp := w.panes[info.id]
		switch {
		case wp == nil:
			// New pane: pipeline plus a seed of its current screen —
			// which is empty for panes born after the Server attached,
			// making them full-fidelity from their first byte.
			wp = &watchedPane{pipe: newPipeline(info.id, info.width, info.height), width: info.width, height: info.height}
			w.panes[info.id] = wp
			w.hub.put(info.id, wp.pipe)
			if err := w.seed(ctx, info.id, wp); err != nil {
				return err
			}
		case wp.width != info.width || wp.height != info.height:
			// Resized pane: tmux reflows content in ways an emulator
			// resize does not reproduce, so resize and re-seed from the
			// authoritative screen.
			wp.width, wp.height = info.width, info.height
			wp.pipe.resize(info.width, info.height)
			if err := w.seed(ctx, info.id, wp); err != nil {
				return err
			}
		}
	}

	// Panes that disappeared from the snapshot have exited.
	for id, wp := range w.panes {
		if !seen[id] {
			delete(w.panes, id)
			w.hub.remove(id)
			wp.pipe.close(true)
		}
	}

	// Keep tmux's own enumeration order: it is stable (window index, then
	// pane index) and matches what the developer sees in tmux itself.
	w.registry.Replace(panes)
	return nil
}

// seed initializes (or re-baselines) a pipeline from the pane's current
// screen: rendered rows with their SGR styling (capture-pane -e), then the
// cursor position. Output events that predate the capture are dropped via
// the seed barrier — their bytes are already on the captured screen — so
// the seed and the live stream compose without duplication or loss.
//
// A seed is the documented degraded reconstruction (ARCHITECTURE.md §5.3):
// exact visible content and styling, but no scrollback and no in-flight
// escape state. Panes created after the Server attached skip the loss: they
// are seeded from an empty screen.
func (w *watcher) seed(ctx context.Context, paneID string, wp *watchedPane) error {
	rows, seq, err := w.ctl.CommandSeq(ctx, "capture-pane", "-p", "-e", "-t", paneID)
	if err != nil {
		return err
	}
	cursor, _, err := w.ctl.CommandSeq(ctx, "display-message", "-p", "-t", paneID, "#{cursor_x}\t#{cursor_y}")
	if err != nil {
		return err
	}
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
	wp.pipe.output([]byte(b.String()))
	return nil
}

// parsePaneLine parses one reconcile line: id, width, height, title,
// tab-separated (titles may contain anything but tabs).
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
	info.title = parts[3]
	return info, nil
}
