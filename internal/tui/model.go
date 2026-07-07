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
}

// clientConn adapts *client.Client to Conn (its WatchPane returns the
// concrete stream type).
type clientConn struct{ c *client.Client }

func (cc clientConn) ListPanes(ctx context.Context) ([]herd.Pane, error) {
	return cc.c.ListPanes(ctx) //nolint:wrapcheck // pure adapter
}

func (cc clientConn) WatchPane(ctx context.Context, paneID string) (PaneStream, error) {
	return cc.c.WatchPane(ctx, paneID) //nolint:wrapcheck // pure adapter
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

	// The watched Pane, once one is picked from the Herd.
	watching   *herd.Pane
	stream     PaneStream
	pane       *grid.Grid
	cursor     grid.Cursor
	paneExited bool
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

// Update handles messages: quit keys, fetch results, and the render stream.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}

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
		return m, recvCmd(m.stream)

	case paneUpdateMsg:
		m.applyUpdate(client.PaneUpdate(msg))
		if m.paneExited {
			return m, nil // stream is over; the Server sends nothing after exited
		}
		return m, recvCmd(m.stream)

	case watchFailedMsg:
		m.err = msg.err
	}
	return m, nil
}

// applyUpdate folds one stream update into the local grid state.
func (m *Model) applyUpdate(u client.PaneUpdate) {
	switch {
	case u.Exited:
		m.paneExited = true
	case u.Resized != nil:
		// A resize resets content; the following damage re-establishes it.
		m.pane = grid.New(u.Resized[0], u.Resized[1])
	case u.Damage != nil:
		if m.pane == nil {
			return // protocol promises a resize first; tolerate its absence
		}
		for _, p := range u.Damage {
			m.pane.Apply(p)
		}
		m.cursor = u.Cursor
	}
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
	case m.paneExited:
		body = statusStyle.Render(fmt.Sprintf("Pane %s exited", m.watching.ID))
	case m.watching != nil && m.pane != nil:
		// The live Pane, full-screen and read-only. Content larger than
		// the terminal is clipped by the renderer; resize-on-focus is the
		// dashboard slice's business.
		view := tea.NewView(renderPane(m.pane, m.cursor))
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
