package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/irodion/shevet/internal/herd"
)

// View renders the dashboard full-screen: the Pane grid, or the focused
// Pane, or one of the whole-dashboard states (connecting, load failure).
func (m Model) View() tea.View {
	var content string
	switch {
	case !m.loaded:
		content = m.centered(stylePlaceholder.Render("Connecting to the Herd..."))
	case m.err != nil:
		content = m.centered(styleError.Render("Cannot reach the Herd: " + m.err.Error()))
	case m.focused:
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

// gridView is the dashboard proper: header, the card grid, footer.
func (m Model) gridView() string {
	availH := max(m.height-headerRows-footerRows, minCardH)

	var body string
	if len(m.panes) == 0 {
		body = "The Herd is empty\n" + styleNote.Render("Spawn or Adopt an Agent on a Host, then press r")
		body = lipgloss.NewStyle().Align(lipgloss.Center).Render(body)
	} else {
		body = m.cardsView()
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		m.headerView(),
		"",
		lipgloss.Place(m.width, availH, lipgloss.Center, lipgloss.Center, body),
		m.footerView(),
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
// screen and the input-stream failure when passthrough is unavailable.
func (m Model) footerView() string {
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

	if n := len(m.panes); n > 0 {
		l := layoutCards(m.width, m.height, n)
		if off := l.rowOffset(m.selected); off > 0 {
			legend = styleNote.Render("▲ more  ") + legend
		} else if l.rows > l.visibleRows {
			legend = styleNote.Render("▼ more  ") + legend
		}
	}
	if m.inputErr != nil {
		legend += sep + styleError.Render("input unavailable: "+m.inputErr.Error())
	}
	return " " + legend
}

// cardsView tiles the Herd into the card grid, scrolled so the selected
// card is always on screen.
func (m Model) cardsView() string {
	l := layoutCards(m.width, m.height, len(m.panes))
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
	for _, row := range m.cardBody(ref, iw, ih) {
		b.WriteString(border.Render("│"))
		b.WriteString(row)
		b.WriteString(border.Render("│"))
		b.WriteByte('\n')
	}
	b.WriteString(cardBottom(border, m.views[ref], ref, iw))
	return b.String()
}

// cardTop renders `╭─ title ────╮`, the title truncated to fit.
func cardTop(border, title lipgloss.Style, label string, iw int) string {
	label = ansi.Truncate(label, max(iw-4, 0), "…")
	fill := max(iw-lipgloss.Width(label)-3, 0)
	return border.Render("╭─ ") + title.Render(label) + border.Render(" "+strings.Repeat("─", fill)+"╮")
}

// cardBottom renders `╰─ badge ───── host:%1 ─╯`: an optional lifecycle
// badge on the left, the PaneRef on the right, either dropped — badge first
// — when the card is too narrow for both.
func cardBottom(border lipgloss.Style, st *paneState, ref herd.PaneRef, iw int) string {
	var badge, refPart string
	var badgeW, refW int

	if label := ansi.Truncate(ref.String(), max(iw-4, 0), "…"); iw >= lipgloss.Width(label)+4 {
		refW = lipgloss.Width(label) + 3
		refPart = border.Render(" ") + styleRef.Render(label) + border.Render(" ─")
	}
	if text, style, ok := lifecycleBadge(st); ok {
		if w := lipgloss.Width(text) + 3; iw >= refW+w+1 {
			badgeW = w
			badge = border.Render("─ ") + style.Render(text) + border.Render(" ")
		}
	}

	fill := strings.Repeat("─", max(iw-badgeW-refW, 0))
	return border.Render("╰") + badge + border.Render(fill) + refPart + border.Render("╯")
}

// lifecycleBadge names the state a card should wear on its frame: nothing
// for a live Pane, "exited" for a Pane whose process ended (the final
// screen stays up), "stream lost" when its render stream broke.
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
