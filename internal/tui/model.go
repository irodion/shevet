// Package tui implements the Client dashboard: the Bubble Tea program run by
// `shevet connect` (docs/adr/0002-pure-go-tui-bubbletea-v2.md).
//
// In this slice the dashboard shows the size of the Herd and quits on demand;
// the Pane grid widget arrives in a later slice.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/irodion/shevet/internal/herd"
)

// fetchTimeout bounds the initial Herd fetch so a wedged Server surfaces as
// an on-screen error instead of a silently empty dashboard.
const fetchTimeout = 5 * time.Second

// PaneLister fetches the Herd from a Server. *client.Client implements it;
// tests substitute fakes.
type PaneLister interface {
	ListPanes(ctx context.Context) ([]herd.Pane, error)
}

// Model is the dashboard's Bubble Tea model. Construct with New.
type Model struct {
	lister PaneLister

	panes  []herd.Pane
	loaded bool
	err    error
}

// New returns a dashboard Model that will populate itself from lister.
func New(lister PaneLister) Model {
	return Model{lister: lister}
}

// Run drives the dashboard to completion on the caller's terminal.
func Run(ctx context.Context, lister PaneLister) error {
	program := tea.NewProgram(New(lister), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run dashboard: %w", err)
	}
	return nil
}

type panesLoadedMsg []herd.Pane

type loadFailedMsg struct{ err error }

// Init kicks off the initial Herd fetch.
func (m Model) Init() tea.Cmd {
	return fetchPanesCmd(m.lister)
}

// fetchPanesCmd closes over only the lister, not the whole Model — commands
// outlive the Model value that issued them, and should not carry it.
func fetchPanesCmd(lister PaneLister) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		panes, err := lister.ListPanes(ctx)
		if err != nil {
			return loadFailedMsg{err: err}
		}
		return panesLoadedMsg(panes)
	}
}

// Update handles messages: quit keys and fetch results.
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
	case loadFailedMsg:
		m.err = msg.err
		m.loaded = true
	}
	return m, nil
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
	var b strings.Builder

	b.WriteString(titleStyle.Render("Shevet"))
	b.WriteString("\n\n")

	switch {
	case !m.loaded:
		b.WriteString(statusStyle.Render("Connecting to Server..."))
	case m.err != nil:
		b.WriteString(errorStyle.Render("Cannot reach Server: " + m.err.Error()))
	default:
		b.WriteString(renderHerdSummary(m.panes))
	}

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
