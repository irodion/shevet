// Package tui implements the Client dashboard: the Bubble Tea program run by
// `shevet connect` (docs/adr/0002-pure-go-tui-bubbletea-v2.md).
//
// The dashboard is the Pane grid (#11): every Pane in the aggregated Herd is
// watched and rendered as a live thumbnail card. The keyboard moves the
// selection; Enter focuses the selected Pane — full-screen, resized to the
// Client viewport, keystrokes passing through — and the leader (Ctrl-\)
// returns to the grid, restoring the Pane's prior size.
//
// The Herd is aggregated across one or more Server connections, each carrying
// a Host alias. Every Pane is keyed by a herd.PaneRef (alias + Server-scoped
// pane id), so panes that share a tmux id across Hosts never collide; input
// and watches route to the owning connection by the PaneRef's Host, while the
// wire still carries the bare pane id (ARCHITECTURE §3.2).
package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
)

// fetchTimeout bounds a Herd fetch so a wedged Server surfaces as an
// on-screen error instead of a silently empty dashboard.
const fetchTimeout = 5 * time.Second

// PaneStream is a live render stream for one Pane, as the dashboard
// consumes it. *client.PaneWatch implements it.
type PaneStream interface {
	Recv() (client.PaneUpdate, error)
}

// Conn is the dashboard's view of a Server connection. *client.Client is
// adapted by Run; tests substitute fakes.
type Conn interface {
	ListPanes(ctx context.Context) ([]herd.Pane, error)
	WatchPane(ctx context.Context, paneID string) (PaneStream, error)
	SendInput(ctx context.Context) (InputSink, error)
}

// InputSink is the Control Input stream as the dashboard drives it: forward
// encoded keystrokes with SendKeys and resize requests with SendResize.
// *client.InputStream implements it.
type InputSink interface {
	SendKeys(paneID string, data []byte) error
	SendResize(paneID string, w, h int) error
}

// clientConn adapts *client.Client to Conn (its stream methods return the
// concrete stream types).
type clientConn struct{ c *client.Client }

func (cc clientConn) ListPanes(ctx context.Context) ([]herd.Pane, error) {
	return cc.c.ListPanes(ctx) //nolint:wrapcheck // pure adapter
}

func (cc clientConn) WatchPane(ctx context.Context, paneID string) (PaneStream, error) {
	return cc.c.WatchPane(ctx, paneID) //nolint:wrapcheck // pure adapter
}

func (cc clientConn) SendInput(ctx context.Context) (InputSink, error) {
	return cc.c.SendInput(ctx) //nolint:wrapcheck // pure adapter
}

// FromClient adapts a dialed *client.Client into a Conn for the dashboard.
func FromClient(c *client.Client) Conn { return clientConn{c: c} }

// Server is one Host connection in the Client's multi-Host view: a Host alias
// paired with the connection reaching its Server. The alias scopes every Pane
// the connection yields into a herd.PaneRef.
type Server struct {
	Alias string
	Conn  Conn
}

// NewServers validates the connection set for one Client invocation: aliases
// must be non-empty and unique, because the Client keys every Pane by PaneRef
// (Host alias + pane id) — a repeated alias would collapse two Hosts' Panes
// into one identity. It returns the servers unchanged on success, or an
// actionable error naming the offending alias.
func NewServers(servers []Server) ([]Server, error) {
	seen := make(map[string]struct{}, len(servers))
	for _, s := range servers {
		if s.Alias == "" {
			return nil, fmt.Errorf("host alias must not be empty")
		}
		if _, dup := seen[s.Alias]; dup {
			return nil, fmt.Errorf("duplicate host alias %q: each Host in one connect invocation needs a distinct alias", s.Alias)
		}
		seen[s.Alias] = struct{}{}
	}
	return servers, nil
}

// refPane is a Pane scoped by the Host alias it was fetched from — the Herd is
// aggregated across connections, so a Pane must carry the alias that resolves
// it back to the connection that owns it. The Client-scoped PaneRef is derived
// from the two on demand, so the pane id is stored once (on Pane).
type refPane struct {
	Host string
	Pane herd.Pane
}

// Ref is the Pane's Client-scoped identity: its Host alias plus its
// Server-scoped id.
func (p refPane) Ref() herd.PaneRef {
	return herd.PaneRef{Host: p.Host, ID: p.Pane.ID}
}

// paneState is the Client-side live state of one watched Pane: the render
// stream folded into a local grid, plus the stream's failure if it broke
// (the last frame stays renderable behind the badge). stream identifies the
// watch this state is fed by — updates from any other stream (a tail
// outliving a refresh, a watch replaced after a failure) are stale and must
// be dropped, never applied.
type paneState struct {
	view   *client.PaneView
	stream PaneStream
	err    error
}

// Model is the dashboard's Bubble Tea model. Construct with New.
type Model struct {
	// servers is the aggregated Herd's source, in display order; conns
	// resolves a PaneRef's Host alias back to the connection that owns it,
	// for watch and input routing.
	servers []Server
	conns   map[string]Conn

	// ctx bounds the connections' streams; commands dial with it so
	// quitting the program tears every watch down.
	ctx context.Context

	// width and height are the Client terminal's viewport — the card grid's
	// canvas, and the size a focused Pane is resized to.
	width, height int

	// The aggregated Herd, in display order, and the live state of every
	// watched Pane, keyed by PaneRef.
	panes  []refPane
	loaded bool
	err    error
	views  map[herd.PaneRef]*paneState

	// selected indexes panes: the card the keyboard is on.
	selected int

	// Focus state. The grid is read-only; focusing a Pane switches to
	// passthrough, where keystrokes are encoded and forwarded and only the
	// leader (Ctrl-\) returns. focus is the focused Pane, zero on the grid
	// (a real PaneRef always carries a Host alias). restore is the focused
	// Pane's pre-focus canonical size, sent back as a resize on unfocus.
	//
	// pending tracks restores that have been sent (or should have been) but
	// not yet confirmed by a PaneResized. It is what makes the pre-focus
	// size survive a quick unfocus→refocus: the Client-side grid still
	// reads the viewport size until the restore round-trips through tmux,
	// so sampling it again would adopt the viewport as the size to restore
	// to — and the Pane's true size would be lost for good.
	//
	// imposed tracks, per Pane, the focus resizes this Client has sent
	// whose confirming PaneResized has not yet arrived (a multiset: the
	// same size can be in flight twice across a refocus). It is how a late
	// echo of our own resize — which can differ from the current viewport
	// if the terminal changed mid-focus — is told apart from the restore
	// landing or a genuinely external resize: only the latter two settle a
	// pending restore.
	focus   herd.PaneRef
	restore grid.Size
	pending map[herd.PaneRef]grid.Size
	imposed map[herd.PaneRef][]grid.Size

	// Input plumbing: one generation of forwarding plumbing per Host (see
	// inputConn), opened lazily on the first focus of one of its Panes and
	// drained onto that Host's Control Input stream by a forwarding Cmd.
	// inputErrs records, per Host, a stream that could not open or broke;
	// focusing a Pane on that Host again retries with a new generation.
	inputs    map[string]*inputConn
	inputErrs map[string]error
}

// focused reports whether a Pane holds the dashboard's focus (passthrough).
func (m Model) focused() bool {
	return m.focus != herd.PaneRef{}
}

// New returns a dashboard Model that will populate itself from the given
// Server connections. Aliases are assumed validated (see NewServers).
func New(ctx context.Context, servers []Server) Model {
	conns := make(map[string]Conn, len(servers))
	for _, s := range servers {
		conns[s.Alias] = s.Conn
	}
	return Model{
		servers:   servers,
		conns:     conns,
		ctx:       ctx,
		views:     make(map[herd.PaneRef]*paneState),
		pending:   make(map[herd.PaneRef]grid.Size),
		imposed:   make(map[herd.PaneRef][]grid.Size),
		inputs:    make(map[string]*inputConn),
		inputErrs: make(map[string]error),
	}
}

// Run drives the dashboard to completion on the caller's terminal, over the
// validated Server connections.
func Run(ctx context.Context, servers []Server) error {
	program := tea.NewProgram(New(ctx, servers), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run dashboard: %w", err)
	}
	return nil
}

type panesLoadedMsg []refPane

type loadFailedMsg struct{ err error }

// Stream messages carry the PaneRef they belong to: every Pane in the Herd
// is watched concurrently, and updates must route to the right card.
type watchStartedMsg struct {
	ref    herd.PaneRef
	stream PaneStream
}

type paneUpdateMsg struct {
	ref    herd.PaneRef
	stream PaneStream
	update client.PaneUpdate
}

type watchFailedMsg struct {
	ref    herd.PaneRef
	stream PaneStream // nil when the watch never opened
	err    error
}

// inputFailedMsg reports that a Host's Control Input stream could not open
// or broke. It names the generation that failed: a retired generation's
// death rattle must not tear down its healthy replacement. Rendering is
// unaffected; focusing retries.
type inputFailedMsg struct {
	host string
	gen  *inputConn
	err  error
}

// Init kicks off the initial Herd fetch.
func (m Model) Init() tea.Cmd {
	return fetchPanesCmd(m.ctx, m.servers)
}

// fetchPanesCmd closes over only what it needs, not the whole Model —
// commands outlive the Model value that issued them, and should not carry it.
// It lists each Server's Panes and tags them with the Host alias, aggregating
// the Herd across connections; a Host's pane ids stay Server-scoped on the
// wire but become PaneRefs Client-side. Any Host's failure fails the load,
// with the alias named so the cause is unambiguous.
func fetchPanesCmd(ctx context.Context, servers []Server) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()

		var all []refPane
		for _, s := range servers {
			panes, err := s.Conn.ListPanes(ctx)
			if err != nil {
				return loadFailedMsg{err: fmt.Errorf("host %s: %w", s.Alias, err)}
			}
			for _, p := range panes {
				all = append(all, refPane{Host: s.Alias, Pane: p})
			}
		}
		return panesLoadedMsg(all)
	}
}

// watchPaneCmd opens the render stream for one Pane on its owning
// connection; the wire carries the bare Server-scoped id (ref.ID), while the
// Model tracks the full PaneRef.
func watchPaneCmd(ctx context.Context, conn Conn, ref herd.PaneRef) tea.Cmd {
	return func() tea.Msg {
		stream, err := conn.WatchPane(ctx, ref.ID)
		if err != nil {
			return watchFailedMsg{ref: ref, err: err}
		}
		return watchStartedMsg{ref: ref, stream: stream}
	}
}

// recvCmd waits for the next update on one Pane's stream. Update re-issues
// it after every received message, forming the per-Pane receive loop. The
// stream rides along on every message as the loop's identity: Update honors
// a message only while this stream is still the one its paneState is fed by.
func recvCmd(ref herd.PaneRef, stream PaneStream) tea.Cmd {
	return func() tea.Msg {
		u, err := stream.Recv()
		if err != nil {
			return watchFailedMsg{ref: ref, stream: stream, err: err}
		}
		return paneUpdateMsg{ref: ref, stream: stream, update: u}
	}
}

// inputReq is one item on a Host's forwarding channel: encoded keystrokes or
// a resize request, always for one Pane. Exactly one of keys and resize is
// set.
type inputReq struct {
	paneID string
	keys   []byte
	resize *grid.Size
}

// inputConn is one generation of a Host's input plumbing: the request queue
// its forwarding Cmd drains, and the cancel that retires the generation.
// Retiring cancels the generation's context — which its Control Input
// stream was opened under — so a wedged send is aborted rather than left
// to drain a stale backlog into a Pane later, and any failure it reports
// afterwards identifies itself and is ignored.
type inputConn struct {
	ch     chan inputReq
	cancel context.CancelFunc
}

// forwardBuffer is the depth of the request queue between the UI goroutine
// and a Host's forwarding Cmd. It only fills if the socket stalls faster
// than a human types — orders of magnitude more headroom than real typing
// needs — and it also holds requests issued before the stream finishes
// opening.
const forwardBuffer = 256

// forwardInputCmd opens one Host's Control Input stream under the
// generation's context and then drains requests from its queue onto it, in
// order, on this one Cmd's goroutine — so a gRPC client stream (not safe
// for concurrent use) has a single owner, and the UI goroutine never blocks
// on the socket. It returns inputFailedMsg (tagged with its generation) if
// the stream cannot open or a send fails, and nothing when the generation
// is retired or the program ends.
func forwardInputCmd(ctx context.Context, conn Conn, host string, gen *inputConn) tea.Cmd {
	return func() tea.Msg {
		sink, err := conn.SendInput(ctx)
		if err != nil {
			return inputFailedMsg{host: host, gen: gen, err: err}
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case req := <-gen.ch:
				if ctx.Err() != nil {
					// Retired while requests were still queued: the backlog
					// is stale input for a driver that gave up on us — it
					// must never replay into the Pane.
					return nil
				}
				// The PaneRef routed us to this Host's connection; the wire
				// carries the bare Server-scoped id.
				var err error
				if req.resize != nil {
					err = sink.SendResize(req.paneID, req.resize.W, req.resize.H)
				} else {
					err = sink.SendKeys(req.paneID, req.keys)
				}
				if err != nil {
					return inputFailedMsg{host: host, gen: gen, err: err}
				}
			}
		}
	}
}

// send hands one request to a forwarding Cmd without ever blocking the UI
// goroutine, reporting whether it was queued. A full buffer means the
// forwarding Cmd has not drained hundreds of requests — the Host's input
// stream has stalled — and blocking on it would freeze the whole dashboard
// (no keys, not even quit). The caller declares the stream stalled instead.
func send(ch chan<- inputReq, req inputReq) bool {
	select {
	case ch <- req:
		return true
	default:
		return false
	}
}

// Update handles messages: keys, viewport changes, fetch results, the
// per-Pane render streams, and the input streams' lifecycle.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.focused() {
			// The viewport is the focused Pane's size; track it. queueResize
			// can drop focus (a stalled stream), so only a resize that was
			// actually queued is recorded as in flight.
			size := grid.Size{W: m.width, H: m.height}
			ref := m.focus
			m.queueResize(ref, size)
			if m.focused() {
				m.impose(ref, size)
			}
		}

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case panesLoadedMsg:
		return m.applyHerd(msg)

	case loadFailedMsg:
		// A failed fetch. Before anything has loaded this is the whole
		// story (View shows it full-screen); once a Herd is on screen it is
		// only a failed refresh — the dashboard and its live streams are
		// untouched, so the error surfaces in the footer instead.
		m.err = msg.err
		m.loaded = true

	case watchStartedMsg:
		st := m.views[msg.ref]
		if st == nil {
			// The Pane left the Herd while the watch was opening. Nothing
			// reads this stream; the Server ends it when the pane exits.
			return m, nil
		}
		st.stream = msg.stream
		return m, recvCmd(msg.ref, msg.stream)

	case paneUpdateMsg:
		st := m.views[msg.ref]
		if st == nil || st.stream != msg.stream {
			// The Pane left the Herd, or this update is the tail of a
			// replaced stream: stale either way, and applying it would
			// interleave two streams' damage into one grid. Drop it and
			// let the loop end.
			return m, nil
		}
		st.view.Apply(msg.update)
		if r := msg.update.Resized; r != nil && !m.consumeImposed(msg.ref, *r) && m.focus != msg.ref {
			// Not one of our own focus resizes echoing back, and we are not
			// driving the Pane: this is the pending restore landing, or an
			// external resize superseding it. Either way the canonical size
			// is trustworthy again, so the bookkeeping has served its
			// purpose.
			//
			// Echoes are matched by size, not by a request id, and that is
			// deliberate: the watcher coalesces notification bursts into
			// one reconcile, so an intermediate resize's confirmation may
			// never be emitted at all — correlation by id would strand the
			// bookkeeping on acks that legitimately never come. The price
			// is a narrow ambiguity: an external resize that exactly equals
			// an in-flight focus size, landing while unfocused and followed
			// by a refocus before the restore settles, is mistaken for our
			// echo, and the next unfocus restores the pre-focus size over
			// it — a wrong-size Pane the next resize corrects, not a wedge.
			// The real fix is the Focus Lease slice (#14): the Server
			// records the pre-focus size at lease acquire and restores it
			// at release, deleting this client-side bookkeeping entirely.
			delete(m.pending, msg.ref)
			delete(m.imposed, msg.ref)
		}
		if st.view.Exited {
			if m.focus == msg.ref {
				// The focused Pane exited under our fingers: back to the
				// grid. Its card keeps the final screen; nothing to restore.
				m.focus = herd.PaneRef{}
			}
			delete(m.pending, msg.ref)
			delete(m.imposed, msg.ref)
			return m, nil // stream is over; the Server sends nothing after exited
		}
		return m, recvCmd(msg.ref, msg.stream)

	case watchFailedMsg:
		st := m.views[msg.ref]
		if st == nil || st.stream != msg.stream {
			return m, nil // a departed Pane's or replaced stream's tail
		}
		st.err = msg.err
		if m.focus == msg.ref {
			// Blind passthrough is a trap: drop to the grid, where the card
			// wears the failure. The Pane itself may be fine, so its size is
			// still restored.
			m.unfocus()
		}

	case inputFailedMsg:
		if m.inputs[msg.host] != msg.gen {
			// A retired generation reporting its own death (the cancel
			// aborted its stream, or it was already superseded): history,
			// not news — the live generation must not be torn down for it.
			return m, nil
		}
		// This Host's passthrough is unavailable; drop back to the grid but
		// keep rendering. Focusing again retries with a fresh generation.
		m.dropInput(msg.host, msg.err)
	}
	return m, nil
}

// dropInput retires a Host's input generation — its stream failed, or its
// queue overflowed because nothing was draining it — and drops back to the
// grid if its Pane was focused. The cancel aborts the generation's stream,
// so a wedged send can neither replay its backlog later nor outlive its
// replacement; the next focus opens a fresh generation. The restore that
// can no longer be sent is recorded as pending, so the next focus/unfocus
// cycle still restores the true pre-focus size.
func (m *Model) dropInput(host string, err error) {
	m.inputErrs[host] = err
	if gen := m.inputs[host]; gen != nil {
		gen.cancel()
		delete(m.inputs, host)
	}
	if m.focus.Host == host {
		m.pending[m.focus] = m.restore
		m.focus = herd.PaneRef{}
	}
}

// impose records a focus resize as in flight, until its PaneResized echoes
// back.
func (m *Model) impose(ref herd.PaneRef, size grid.Size) {
	m.imposed[ref] = append(m.imposed[ref], size)
}

// consumeImposed removes one in-flight focus resize matching an arriving
// PaneResized, reporting whether the resize was our own echo.
func (m *Model) consumeImposed(ref herd.PaneRef, size grid.Size) bool {
	for i, s := range m.imposed[ref] {
		if s == size {
			m.imposed[ref] = append(m.imposed[ref][:i], m.imposed[ref][i+1:]...)
			if len(m.imposed[ref]) == 0 {
				delete(m.imposed, ref)
			}
			return true
		}
	}
	return false
}

// unfocus returns to the grid, sending the focused Pane its pre-focus size
// and remembering it as pending until a PaneResized confirms it landed.
func (m *Model) unfocus() {
	m.queueResize(m.focus, m.restore)
	m.pending[m.focus] = m.restore
	m.focus = herd.PaneRef{}
}

// applyHerd folds a (re)fetched Herd into the model: Panes still present
// keep their live state, new ones start watching, Panes whose stream broke
// start over with a fresh watch (this is what makes `r` the recovery
// gesture for a "stream lost" card), gone ones are dropped. The selection
// follows the Pane it was on, falling back to the first card.
func (m Model) applyHerd(panes []refPane) (tea.Model, tea.Cmd) {
	var selectedRef herd.PaneRef
	if m.selected < len(m.panes) {
		selectedRef = m.panes[m.selected].Ref()
	}

	m.panes = panes
	m.loaded = true
	m.err = nil
	m.selected = 0

	live := make(map[herd.PaneRef]bool, len(panes))
	var cmds []tea.Cmd
	for i, p := range panes {
		ref := p.Ref()
		live[ref] = true
		if ref == selectedRef {
			m.selected = i
		}
		if st := m.views[ref]; st == nil || st.err != nil {
			// A fresh paneState: its stream field is what makes any tail of
			// the replaced stream stale, so the old and new watch can never
			// interleave damage into one grid.
			m.views[ref] = &paneState{view: client.NewPaneView()}
			cmds = append(cmds, watchPaneCmd(m.ctx, m.conns[p.Host], ref))
		}
	}
	for ref := range m.views {
		if !live[ref] {
			delete(m.views, ref)
			delete(m.pending, ref)
			delete(m.imposed, ref)
		}
	}
	if m.focused() && !live[m.focus] {
		m.focus = herd.PaneRef{}
	}
	return m, tea.Batch(cmds...)
}

// handleKey routes a key press by mode. On the grid the dashboard owns the
// keys (navigation, focus, refresh, quit); in passthrough every key is
// encoded and forwarded to the focused Pane except the reserved leader,
// which returns to the grid and restores the Pane's pre-focus size.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.focused() {
		if isLeader(msg) {
			m.unfocus()
			return m, nil
		}
		if data := encodeKey(msg); len(data) > 0 {
			if gen := m.inputs[m.focus.Host]; gen != nil {
				if !send(gen.ch, inputReq{paneID: m.focus.ID, keys: data}) {
					m.dropInput(m.focus.Host, errStalledInput)
				}
			}
		}
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "left", "h":
		m.moveSelection(-1, 0)
	case "right", "l":
		m.moveSelection(1, 0)
	case "up", "k":
		m.moveSelection(0, -1)
	case "down", "j":
		m.moveSelection(0, 1)
	case "r":
		return m, fetchPanesCmd(m.ctx, m.servers)
	case "enter", "i":
		return m.focusSelected()
	}
	return m, nil
}

// moveSelection moves the grid selection one card in the given direction,
// stopping at the grid's edges (no wrap: the edge is a landmark).
func (m *Model) moveSelection(dx, dy int) {
	n := len(m.panes)
	if n == 0 {
		return
	}
	l := layoutCards(m.width, m.height, n)
	col, row := m.selected%l.cols+dx, m.selected/l.cols+dy
	if col < 0 || col >= l.cols || row < 0 {
		return
	}
	if next := row*l.cols + col; next < n {
		m.selected = next
	}
}

// focusSelected enters the selected Pane: passthrough input, full-screen
// view, and the Pane resized to the Client viewport. The Pane's canonical
// size at this moment is remembered and restored on unfocus. Focusing needs
// a live, sized Pane — one whose render stream is up, or passthrough would
// be typing into a frozen frame — and a known viewport; otherwise the key
// is ignored.
func (m Model) focusSelected() (Model, tea.Cmd) {
	if m.selected >= len(m.panes) || m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	ref := m.panes[m.selected].Ref()
	st := m.views[ref]
	if st == nil || st.err != nil || st.view.Exited || !sized(st.view.Grid) {
		return m, nil
	}

	m.focus = ref
	if p, ok := m.pending[ref]; ok {
		// A restore is still in flight for this Pane, so the grid reads a
		// size we imposed, not the Pane's own. The pending target is the
		// true pre-focus size; keep restoring to it.
		m.restore = p
	} else {
		w, h := st.view.Grid.Size()
		m.restore = grid.Size{W: w, H: h}
	}

	// Open this Host's input generation when there isn't a live one —
	// including a retry after a prior failure. Requests issued before the
	// stream opens wait in the buffer.
	var cmd tea.Cmd
	gen := m.inputs[ref.Host]
	if gen == nil {
		delete(m.inputErrs, ref.Host)
		genCtx, cancel := context.WithCancel(m.ctx)
		gen = &inputConn{ch: make(chan inputReq, forwardBuffer), cancel: cancel}
		m.inputs[ref.Host] = gen
		cmd = forwardInputCmd(genCtx, m.conns[ref.Host], ref.Host, gen)
	}
	viewport := grid.Size{W: m.width, H: m.height}
	if !send(gen.ch, inputReq{paneID: ref.ID, resize: &viewport}) {
		// A reused queue can be full (a stalled stream); a focus that
		// cannot even ask for its size does not happen.
		m.dropInput(ref.Host, errStalledInput)
		return m, cmd
	}
	m.impose(ref, viewport)
	return m, cmd
}

// errStalledInput reports an input queue that overflowed: the forwarding
// Cmd stopped draining it, which means the Control Input stream is wedged.
var errStalledInput = errors.New("input stream stalled; focus a Pane to reconnect")

// queueResize hands a resize request for ref to its Host's forwarding Cmd,
// if that Host's input stream is up (it always is on the focus/unfocus
// paths, which open it; after an input failure the restore is skipped —
// there is no stream left to carry it). A stalled stream is dropped rather
// than blocked on.
func (m *Model) queueResize(ref herd.PaneRef, size grid.Size) {
	if gen := m.inputs[ref.Host]; gen != nil {
		if !send(gen.ch, inputReq{paneID: ref.ID, resize: &size}) {
			m.dropInput(ref.Host, errStalledInput)
		}
	}
}

// isLeader reports whether msg is the reserved passthrough leader (Ctrl-\),
// the one chord that returns from passthrough instead of reaching the Pane.
func isLeader(msg tea.KeyPressMsg) bool {
	k := tea.Key(msg)
	return k.Mod&tea.ModCtrl != 0 && k.Code == '\\'
}

// sized reports whether a stream's initial resize has arrived and the grid
// is renderable.
func sized(g *grid.Grid) bool {
	w, _ := g.Size()
	return w > 0
}
