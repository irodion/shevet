// Package tui implements the Client dashboard: the Bubble Tea program run by
// `shevet connect` (docs/adr/0002-pure-go-tui-bubbletea-v2.md).
//
// In this slice the dashboard shows one live Pane: it watches the first Pane
// in the aggregated Herd full-screen, read-only. The Pane grid widget with
// navigation arrives with the dashboard slice (#11).
//
// The Herd is aggregated across one or more Server connections, each carrying
// a Host alias. Every Pane is keyed by a herd.PaneRef (alias + Server-scoped
// pane id), so panes that share a tmux id across Hosts never collide; input
// and watches route to the owning connection by the PaneRef's Host, while the
// wire still carries the bare pane id (ARCHITECTURE §3.2).
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
)

// fetchTimeout bounds the initial Herd fetch so a wedged Server surfaces as
// an on-screen error instead of a silently empty dashboard.
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
// encoded keystrokes with SendKeys. *client.InputStream implements it.
type InputSink interface {
	SendKeys(paneID string, data []byte) error
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

// refPane couples a Pane with its Client-scoped PaneRef — the Herd is
// aggregated across connections, so a Pane must carry the Host alias that
// resolves it back to the connection that owns it.
type refPane struct {
	Ref  herd.PaneRef
	Pane herd.Pane
}

// Model is the dashboard's Bubble Tea model. Construct with New.
type Model struct {
	// servers is the aggregated Herd's source, in display order; conns
	// resolves a PaneRef's Host alias back to the connection that owns it,
	// for watch and input routing.
	servers []Server
	conns   map[string]Conn

	// ctx bounds the connections' streams; commands dial with it so
	// quitting the program tears the watch down.
	ctx context.Context

	panes  []refPane
	loaded bool
	err    error

	// The watched Pane, once one is picked from the Herd; view folds the
	// render stream into the local grid.
	watching *refPane
	stream   PaneStream
	view     *client.PaneView

	// Input state. The base view is read-only (like the dashboard); focused
	// switches to passthrough, where keystrokes are encoded and forwarded to
	// the Pane and only the leader (Ctrl-\) is reserved to return. Entering
	// passthrough allocates input and hands it to a forwarding Cmd that opens
	// the Control Input stream and drains the channel onto it in order;
	// keystrokes typed before the stream opens wait in the buffer. inputErr
	// records a stream that could not open or broke (passthrough is then
	// unavailable).
	focused  bool
	input    chan []byte
	inputErr error
}

// New returns a dashboard Model that will populate itself from the given
// Server connections. Aliases are assumed validated (see NewServers).
func New(ctx context.Context, servers []Server) Model {
	conns := make(map[string]Conn, len(servers))
	for _, s := range servers {
		conns[s.Alias] = s.Conn
	}
	return Model{servers: servers, conns: conns, ctx: ctx}
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

type watchStartedMsg struct{ stream PaneStream }

type paneUpdateMsg client.PaneUpdate

type watchFailedMsg struct{ err error }

// inputFailedMsg reports that passthrough input is unavailable — the stream
// could not open, or a forwarded keystroke failed. Rendering is unaffected.
type inputFailedMsg struct{ err error }

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
				all = append(all, refPane{Ref: herd.PaneRef{Host: s.Alias, ID: p.ID}, Pane: p})
			}
		}
		return panesLoadedMsg(all)
	}
}

// watchPaneCmd opens the render stream for the chosen Pane on its owning
// connection; the wire carries the bare Server-scoped id (ref.ID), while the
// Model tracks the full PaneRef.
func watchPaneCmd(ctx context.Context, conn Conn, ref herd.PaneRef) tea.Cmd {
	return func() tea.Msg {
		stream, err := conn.WatchPane(ctx, ref.ID)
		if err != nil {
			return watchFailedMsg{err: err}
		}
		return watchStartedMsg{stream: stream}
	}
}

// recvCmd waits for the next update on the stream. Update re-issues it
// after every received message, forming the receive loop.
func recvCmd(stream PaneStream) tea.Cmd {
	return func() tea.Msg {
		u, err := stream.Recv()
		if err != nil {
			return watchFailedMsg{err: err}
		}
		return paneUpdateMsg(u)
	}
}

// forwardBuffer is the depth of the keystroke queue between the UI goroutine
// and the forwarding Cmd. It only fills if the socket stalls faster than a
// human types — orders of magnitude more headroom than real typing needs —
// and it also holds keystrokes typed before the stream finishes opening.
const forwardBuffer = 256

// forwardInputCmd opens the Control Input stream and then drains keystrokes
// from ch onto it, in order, on this one Cmd's goroutine — so a gRPC client
// stream (not safe for concurrent use) has a single owner, and the UI
// goroutine never blocks on the socket. It returns inputFailedMsg if the
// stream cannot open or a send fails, and nothing when the program ends.
func forwardInputCmd(ctx context.Context, conn Conn, ref herd.PaneRef, ch <-chan []byte) tea.Cmd {
	return func() tea.Msg {
		sink, err := conn.SendInput(ctx)
		if err != nil {
			return inputFailedMsg{err: err}
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case data := <-ch:
				// The PaneRef routed us to this Host's connection; the wire
				// carries the bare Server-scoped id.
				if err := sink.SendKeys(ref.ID, data); err != nil {
					return inputFailedMsg{err: err}
				}
			}
		}
	}
}

// sendKey hands one encoded keystroke to the forwarding Cmd, giving up only if
// the program is shutting down (so a stalled socket cannot wedge the UI).
func sendKey(ctx context.Context, ch chan<- []byte, data []byte) {
	select {
	case ch <- data:
	case <-ctx.Done():
	}
}

// Update handles messages: keys, fetch results, the render stream, and the
// input stream's lifecycle.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case panesLoadedMsg:
		m.panes = msg
		m.loaded = true
		m.err = nil
		// This slice views one Pane: the aggregated Herd's first. The grid
		// dashboard slice (#11) replaces this with PaneRef-keyed navigation.
		if len(m.panes) > 0 {
			pane := m.panes[0]
			m.watching = &pane
			return m, watchPaneCmd(m.ctx, m.conns[pane.Ref.Host], pane.Ref)
		}

	case loadFailedMsg:
		m.err = msg.err
		m.loaded = true

	case watchStartedMsg:
		m.stream = msg.stream
		m.view = client.NewPaneView()
		return m, recvCmd(m.stream)

	case paneUpdateMsg:
		m.view.Apply(client.PaneUpdate(msg))
		if m.view.Exited {
			return m, nil // stream is over; the Server sends nothing after exited
		}
		return m, recvCmd(m.stream)

	case watchFailedMsg:
		m.err = msg.err

	case inputFailedMsg:
		// Passthrough is unavailable; drop back to read-only but keep
		// rendering. The error is not fatal to the dashboard.
		m.inputErr = msg.err
		m.focused = false
		m.input = nil
	}
	return m, nil
}

// handleKey routes a key press by mode. In read-only the dashboard owns the
// keys (quit, and entering passthrough); in passthrough every key is encoded
// and forwarded to the Pane except the reserved leader, which returns.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.focused {
		if isLeader(msg) {
			m.focused = false
			return m, nil
		}
		if data := encodeKey(msg); len(data) > 0 && m.input != nil {
			sendKey(m.ctx, m.input, data)
		}
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "i", "enter":
		if !m.canFocus() {
			return m, nil
		}
		m.focused = true
		// Open the stream when there isn't a live one — including a retry
		// after a prior failure. Entering passthrough must never leave input
		// nil, or keystrokes (and the read-only quit keys) would be swallowed
		// with no way back except the leader.
		if m.input == nil {
			m.inputErr = nil
			m.input = make(chan []byte, forwardBuffer)
			ref := m.watching.Ref
			return m, forwardInputCmd(m.ctx, m.conns[ref.Host], ref, m.input)
		}
	}
	return m, nil
}

// canFocus reports whether a live Pane is on screen to type into.
func (m Model) canFocus() bool {
	return m.watching != nil && m.view != nil && !m.view.Exited && sized(m.view.Grid)
}

// isLeader reports whether msg is the reserved passthrough leader (Ctrl-\),
// the one chord that returns from passthrough instead of reaching the Pane.
func isLeader(msg tea.KeyPressMsg) bool {
	k := tea.Key(msg)
	return k.Mod&tea.ModCtrl != 0 && k.Code == '\\'
}

// Styles are package-level for now; a theme arrives with the grid widget.
var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	statusStyle = lipgloss.NewStyle().Faint(true)
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	helpStyle   = lipgloss.NewStyle().Faint(true)
)

// View renders the dashboard full-screen.
func (m Model) View() tea.View {
	var body string
	switch {
	case !m.loaded:
		body = statusStyle.Render("Connecting to Server...")
	case m.err != nil:
		body = errorStyle.Render("Cannot reach Server: " + m.err.Error())
	case m.view != nil && m.view.Exited:
		body = statusStyle.Render(fmt.Sprintf("Pane %s exited", m.watching.Ref))
	case m.view != nil && sized(m.view.Grid):
		// The live Pane, full-screen and read-only. Content larger than
		// the terminal is clipped by the renderer; resize-on-focus is the
		// dashboard slice's business.
		view := tea.NewView(renderPane(m.view.Grid, m.view.Cursor))
		view.AltScreen = true
		return view
	case m.watching != nil:
		body = statusStyle.Render("Attaching to Pane " + m.watching.Ref.String() + "...")
	default:
		body = renderHerdSummary(m.panes)
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("Shevet"))
	b.WriteString("\n\n")
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString(helpStyle.Render("q quit"))

	view := tea.NewView(b.String())
	view.AltScreen = true
	return view
}

// sized reports whether the stream's initial resize has arrived and the
// grid is renderable.
func sized(g *grid.Grid) bool {
	w, _ := g.Size()
	return w > 0
}

// renderHerdSummary renders the aggregated Herd as a count plus one line per
// Pane, each named by its PaneRef so panes from different Hosts stay distinct.
func renderHerdSummary(panes []refPane) string {
	var b strings.Builder

	noun := "Panes"
	if len(panes) == 1 {
		noun = "Pane"
	}
	fmt.Fprintf(&b, "%d %s", len(panes), noun)

	for _, p := range panes {
		fmt.Fprintf(&b, "\n  %s  %s", p.Ref, p.Pane.Title)
	}
	return b.String()
}
