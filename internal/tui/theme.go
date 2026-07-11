package tui

import "charm.land/lipgloss/v2"

// The dashboard theme: one place for every color and style (ADR-0002 —
// Crush-calibre theming in pure Bubble Tea).
//
// Colors are truecolor RGB; the Bubble Tea v2 renderer detects the
// terminal's profile and downsamples for 256-color (and dimmer) terminals,
// so degradation costs nothing here. Body text deliberately stays the
// terminal's default foreground — only accents, chrome, and de-emphasis get
// explicit colors — so the dashboard reads correctly on both dark and light
// backgrounds.
var (
	// colAccent is the brand violet: the selection color and the brand chip.
	colAccent = lipgloss.Color("#7C6AEF")

	// colMuted de-emphasizes chrome: help text, counts, PaneRefs.
	colMuted = lipgloss.Color("#8A8FA3")

	// colBorder is the resting card border, quiet enough that thumbnails —
	// not frames — carry the screen.
	colBorder = lipgloss.Color("#4A4D5E")

	// colWarn and colBad tag Pane lifecycle badges (exited, stream lost).
	colWarn = lipgloss.Color("#D7A65F")
	colBad  = lipgloss.Color("#E06C75")
)

var (
	// styleBrand is the header chip: inverse block, the one place the accent
	// is a background.
	styleBrand = lipgloss.NewStyle().Background(colAccent).Foreground(lipgloss.Color("#F5F3FF")).Bold(true).Padding(0, 1)

	// styleNote renders secondary chrome text (header counts, footer labels).
	styleNote = lipgloss.NewStyle().Foreground(colMuted)

	// styleKey renders the key glyphs in the footer hints, default foreground
	// so they stand out from their muted labels.
	styleKey = lipgloss.NewStyle().Bold(true)

	// Card frames: resting vs selected.
	styleBorder    = lipgloss.NewStyle().Foreground(colBorder)
	styleBorderSel = lipgloss.NewStyle().Foreground(colAccent)

	// Card titles: resting titles are muted so a wall of cards stays calm;
	// the selected title is bold accent — the eye lands exactly once.
	styleTitle    = lipgloss.NewStyle().Foreground(colMuted)
	styleTitleSel = lipgloss.NewStyle().Foreground(colAccent).Bold(true)

	// styleRef renders a card's PaneRef in the bottom border.
	styleRef = lipgloss.NewStyle().Foreground(colMuted)

	// Lifecycle badges in card borders.
	styleBadgeExited = lipgloss.NewStyle().Foreground(colWarn)
	styleBadgeLost   = lipgloss.NewStyle().Foreground(colBad)

	// stylePlaceholder fills a card whose stream has not established yet.
	stylePlaceholder = lipgloss.NewStyle().Foreground(colMuted).Italic(true)

	// styleError renders fatal, whole-dashboard failures.
	styleError = lipgloss.NewStyle().Foreground(colBad)
)
