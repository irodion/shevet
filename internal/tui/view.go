package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/irodion/shevet/internal/herd"
)

// View renders the dashboard full-screen: the Pane grid, or the focused
// Pane, or one of the whole-dashboard states (connecting, load failure).
// A fetch failure is full-screen only while there is no Herd to show; once
// cards are up, a failed refresh reports in the footer — never by blanking
// live Panes.
func (m Model) View() tea.View {
	var content string
	switch {
	case !m.loaded:
		content = m.centered(stylePlaceholder.Render("Connecting to the Herd..."))
	case m.err != nil && len(m.panes) == 0:
		content = m.centered(styleError.Render("Cannot reach the Herd: " + m.err.Error()))
	case m.focused():
		content = m.focusView()
	default:
		content = m.gridView()
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

// centered places s mid-viewport once the terminal size is known.
func (m Model) centered(s string) string {
	if m.width <= 0 || m.height <= 0 {
		return s
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, s)
}

// focusView is the focused Pane, full-screen and chrome-free: the Pane is
// resized to the viewport, so past the transition the grid is exactly the
// screen. During the transition (or if tmux refuses the size) the canonical
// grid renders clipped top-left, and the terminal's own background fills
// the rest.
func (m Model) focusView() string {
	st := m.views[m.focus]
	if st == nil {
		return ""
	}
	gw, gh := st.view.Grid.Size()
	return renderRegion(st.view.Grid, st.view.Cursor, 0, 0, min(gw, m.width), min(gh, m.height))
}

// gridView is the dashboard proper: header, the card grid, footer. The
// layout is computed once per frame and shared, so the cards and the
// footer's scroll markers can never disagree about what is on screen.
func (m Model) gridView() string {
	l := layoutCards(m.width, m.height, len(m.panes))

	var body string
	if len(m.panes) == 0 {
		body = "The Herd is empty\n" + styleNote.Render("Spawn or Adopt an Agent on a Host, then press r")
		body = lipgloss.NewStyle().Align(lipgloss.Center).Render(body)
	} else {
		body = m.cardsView(l)
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		m.headerView(),
		"",
		lipgloss.Place(m.width, l.availH, lipgloss.Center, lipgloss.Center, body),
		m.footerView(l),
	)
}

// headerView is the brand chip plus the Herd's shape at a glance.
func (m Model) headerView() string {
	counts := fmt.Sprintf("%d %s · %d %s",
		len(m.panes), plural(len(m.panes), "Pane", "Panes"),
		len(m.servers), plural(len(m.servers), "Host", "Hosts"))
	return " " + styleBrand.Render("Shevet") + "  " + styleNote.Render(counts)
}

// footerView is the key legend, plus scroll markers when card rows are off
// screen, failed-refresh notice, and input-stream failures when passthrough
// is unavailable.
func (m Model) footerView(l cardLayout) string {
	hint := func(key, action string) string {
		return styleKey.Render(key) + " " + styleNote.Render(action)
	}
	sep := styleNote.Render("  ·  ")
	legend := strings.Join([]string{
		hint("↑↓←→", "select"),
		hint("enter", "focus"),
		hint("ctrl+\\", "unfocus"),
		hint("r", "refresh"),
		hint("q", "quit"),
	}, sep)

	if len(m.panes) > 0 {
		off := l.rowOffset(m.selected)
		var markers string
		if off > 0 {
			markers += "▲ "
		}
		if off+l.visibleRows < l.rows {
			markers += "▼ "
		}
		if markers != "" {
			legend = styleNote.Render(markers+"more  ") + legend
		}
	}
	if m.err != nil {
		legend += sep + styleError.Render("refresh failed: "+m.err.Error())
	}
	for _, host := range slices.Sorted(maps.Keys(m.inputErrs)) {
		legend += sep + styleError.Render(fmt.Sprintf("input unavailable (%s): %v", host, m.inputErrs[host]))
	}
	return " " + legend
}

// cardsView tiles the Herd into the card grid, scrolled so the selected
// card is always on screen.
func (m Model) cardsView(l cardLayout) string {
	off := l.rowOffset(m.selected)

	gutterCol := strings.Repeat(" ", gutterX)
	rows := make([]string, 0, l.visibleRows)
	for r := off; r < min(off+l.visibleRows, l.rows); r++ {
		cards := make([]string, 0, 2*l.cols)
		for c := 0; c < l.cols; c++ {
			i := r*l.cols + c
			if i >= len(m.panes) {
				break
			}
			if c > 0 {
				cards = append(cards, gutterCol)
			}
			p := m.panes[i]
			cards = append(cards, m.renderCard(p, l.cardW, l.cardH, i == m.selected))
		}
		if r > off && gutterY > 0 {
			rows = append(rows, strings.Repeat("\n", gutterY-1))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// renderCard draws one Pane card: the title in the top border, the live
// thumbnail as the body, and the PaneRef — plus a lifecycle badge when the
// Pane has one — in the bottom border. Selection is carried by the accent
// border and title, so exactly one card on the grid draws the eye.
func (m Model) renderCard(p refPane, w, h int, selected bool) string {
	ref := p.Ref()
	iw, ih := w-2, h-2

	border, title := styleBorder, styleTitle
	if selected {
		border, title = styleBorderSel, styleTitleSel
	}

	label := p.Pane.Title
	if label == "" {
		label = ref.ID
	}

	var b strings.Builder
	b.WriteString(cardTop(border, title, label, iw))
	b.WriteByte('\n')
	edge := border.Render("│")
	for _, row := range m.cardBody(ref, iw, ih) {
		b.WriteString(edge)
		b.WriteString(row)
		b.WriteString(edge)
		b.WriteByte('\n')
	}
	b.WriteString(cardBottom(border, m.views[ref], ref, iw))
	return b.String()
}

// cardTop renders `╭─ title ────╮`, the title truncated to fit. Border rows
// are measured (lipgloss.Width of the decorated segments), never offset-
// counted, so changing a decoration cannot desync the width math.
func cardTop(border, title lipgloss.Style, label string, iw int) string {
	head := border.Render("─ ") + title.Render(ansi.Truncate(label, max(iw-4, 0), "…")) + border.Render(" ")
	fill := strings.Repeat("─", max(iw-lipgloss.Width(head), 0))
	return border.Render("╭") + head + border.Render(fill+"╮")
}

// cardBottom renders `╰─ badge ───── host:%1 ─╯`: an optional lifecycle
// badge on the left, the PaneRef on the right, either dropped — badge first
// — when the card is too narrow for both.
func cardBottom(border lipgloss.Style, st *paneState, ref herd.PaneRef, iw int) string {
	var lead, tail string
	if label := ansi.Truncate(ref.String(), max(iw-4, 0), "…"); iw >= lipgloss.Width(label)+4 {
		tail = border.Render(" ") + styleRef.Render(label) + border.Render(" ─")
	}
	if text, style, ok := lifecycleBadge(st); ok {
		candidate := border.Render("─ ") + style.Render(text) + border.Render(" ")
		if iw >= lipgloss.Width(candidate)+lipgloss.Width(tail)+1 {
			lead = candidate
		}
	}

	fill := strings.Repeat("─", max(iw-lipgloss.Width(lead)-lipgloss.Width(tail), 0))
	return border.Render("╰") + lead + border.Render(fill) + tail + border.Render("╯")
}

// lifecycleBadge names the condition a card should wear on its frame:
// nothing for a live Pane, "exited" for a Pane whose process ended (the
// final screen stays up), "stream lost" when its render stream broke.
func lifecycleBadge(st *paneState) (string, lipgloss.Style, bool) {
	switch {
	case st == nil:
		return "", lipgloss.Style{}, false
	case st.err != nil:
		return "stream lost", styleBadgeLost, true
	case st.view.Exited:
		return "exited", styleBadgeExited, true
	default:
		return "", lipgloss.Style{}, false
	}
}

// cardBody renders a card's interior: the Pane's live thumbnail, cropped to
// the rows around the cursor — where an Agent's prompt and freshest output
// live — or a placeholder while the stream establishes.
func (m Model) cardBody(ref herd.PaneRef, iw, ih int) []string {
	st := m.views[ref]
	if st == nil || !sized(st.view.Grid) {
		text := stylePlaceholder.Render("connecting…")
		if st != nil && st.err != nil {
			text = stylePlaceholder.Render("no signal")
		}
		return strings.Split(lipgloss.Place(iw, ih, lipgloss.Center, lipgloss.Center, text), "\n")
	}

	_, gh := st.view.Grid.Size()
	y0 := clamp(st.view.Cursor.Y-ih+1, 0, max(gh-ih, 0))
	return strings.Split(renderRegion(st.view.Grid, st.view.Cursor, 0, y0, iw, ih), "\n")
}

// plural picks the noun form for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
