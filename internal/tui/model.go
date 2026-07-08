// Package tui implements the Client dashboard: the Bubble Tea program run by
// `shevet connect` (docs/adr/0002-pure-go-tui-bubbletea-v2.md).
//
// In this slice the dashboard shows one live Pane: it watches the first Pane
// in the Herd full-screen, read-only. The Pane grid widget with navigation
// arrives with the dashboard slice.
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

// Model is the dashboard's Bubble Tea model. Construct with New.
type Model struct {
	conn Conn

	// ctx bounds the connection's streams; commands dial with it so
	// quitting the program tears the watch down.
	ctx context.Context

	panes  []herd.Pane
	loaded bool
	err    error

	// The watched Pane, once one is picked from the Herd; view folds the
	// render stream into the local grid.
	watching *herd.Pane
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

// New returns a dashboard Model that will populate itself from conn.
func New(ctx context.Context, conn Conn) Model {
	return Model{conn: conn, ctx: ctx}
}

// Run drives the dashboard to completion on the caller's terminal.
func Run(ctx context.Context, c *client.Client) error {
	program := tea.NewProgram(New(ctx, clientConn{c: c}), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run dashboard: %w", err)
	}
	return nil
}

type panesLoadedMsg []herd.Pane

type loadFailedMsg struct{ err error }

type watchStartedMsg struct{ stream PaneStream }

type paneUpdateMsg client.PaneUpdate

type watchFailedMsg struct{ err error }

// inputFailedMsg reports that passthrough input is unavailable — the stream
// could not open, or a forwarded keystroke failed. Rendering is unaffected.
type inputFailedMsg struct{ err error }

// Init kicks off the initial Herd fetch.
func (m Model) Init() tea.Cmd {
	return fetchPanesCmd(m.ctx, m.conn)
}

// fetchPanesCmd closes over only what it needs, not the whole Model —
// commands outlive the Model value that issued them, and should not carry it.
func fetchPanesCmd(ctx context.Context, conn Conn) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()

		panes, err := conn.ListPanes(ctx)
		if err != nil {
			return loadFailedMsg{err: err}
		}
		return panesLoadedMsg(panes)
	}
}

// watchPaneCmd opens the render stream for the chosen Pane.
func watchPaneCmd(ctx context.Context, conn Conn, paneID string) tea.Cmd {
	return func() tea.Msg {
		stream, err := conn.WatchPane(ctx, paneID)
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
func forwardInputCmd(ctx context.Context, conn Conn, paneID string, ch <-chan []byte) tea.Cmd {
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
				if err := sink.SendKeys(paneID, data); err != nil {
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
		// This slice views one Pane: the Herd's first. The grid dashboard
		// slice replaces this with navigation.
		if len(m.panes) > 0 {
			pane := m.panes[0]
			m.watching = &pane
			return m, watchPaneCmd(m.ctx, m.conn, pane.ID)
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
		if m.input == nil && m.inputErr == nil {
			m.input = make(chan []byte, forwardBuffer)
			return m, forwardInputCmd(m.ctx, m.conn, m.watching.ID, m.input)
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
		body = statusStyle.Render(fmt.Sprintf("Pane %s exited", m.watching.ID))
	case m.view != nil && sized(m.view.Grid):
		// The live Pane, full-screen and read-only. Content larger than
		// the terminal is clipped by the renderer; resize-on-focus is the
		// dashboard slice's business.
		view := tea.NewView(renderPane(m.view.Grid, m.view.Cursor))
		view.AltScreen = true
		return view
	case m.watching != nil:
		body = statusStyle.Render("Attaching to Pane " + m.watching.ID + "...")
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

// renderHerdSummary renders the Herd as a count plus one line per Pane.
func renderHerdSummary(panes []herd.Pane) string {
	var b strings.Builder

	noun := "Panes"
	if len(panes) == 1 {
		noun = "Pane"
	}
	fmt.Fprintf(&b, "%d %s", len(panes), noun)

	for _, p := range panes {
		fmt.Fprintf(&b, "\n  %s  %s", p.ID, p.Title)
	}
	return b.String()
}
