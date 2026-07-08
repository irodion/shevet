package tui

import (
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// encodeKey turns a parsed key press back into the terminal bytes that
// produced it, so the Client can forward keystrokes to a Pane as if typed
// there directly. It is the inverse of the terminal input parsing Bubble Tea
// did on the way in, targeting the conventional (legacy/xterm) encoding that
// tmux forwards and inner applications expect.
//
// The result is the exact byte sequence: printable text (including multi-byte
// UTF-8) as itself, Enter as CR, control chords as their C0 byte, Esc and the
// navigation keys as their escape sequences, with an Alt/Meta press adding the
// leading ESC. A key that carries no bytes (a lone modifier, an unmapped
// function key) returns nil, and the caller sends nothing.
//
// Scope for this slice (ARCHITECTURE.md §3.2): the keys an interactive shell
// or agent needs — text, Enter, Ctrl chords, Esc, Backspace, Tab, and the
// arrow/navigation cluster. Modified navigation keys (Ctrl+Arrow) and the
// function-key row encode their base sequence; the full modifier-parameter
// and application-cursor-mode matrix is a later refinement, not a v1 gate.
func encodeKey(msg tea.KeyPressMsg) []byte {
	key := tea.Key(msg)

	// Alt (Meta) is expressed as an ESC prefix on top of the base encoding.
	var out []byte
	if key.Mod&tea.ModAlt != 0 {
		out = append(out, 0x1b)
	}

	// Named keys (Enter, Esc, arrows, ...) take priority over their rune
	// value: KeyEnter's Code is CR, but routing it here keeps the intent
	// explicit and the table in one place.
	if seq, ok := namedKey(key.Code); ok {
		return append(out, seq...)
	}

	// Ctrl chords collapse a printable base key to its C0 control byte —
	// checked before Text, since a platform that also fills Text for a chord
	// (e.g. "c" for Ctrl-C) must still yield 0x03, not "c".
	if key.Mod&tea.ModCtrl != 0 {
		if b, ok := ctrlByte(key.Code); ok {
			return append(out, b)
		}
	}

	// Printable input: exactly what the user typed. Text already reflects
	// shifting and the keyboard layout, and carries whole UTF-8 graphemes.
	if key.Text != "" {
		return append(out, key.Text...)
	}

	// A bare printable rune with no Text (uncommon, but possible for keys the
	// parser didn't attach text to).
	if key.Code != 0 && key.Code < tea.KeyExtended && unicode.IsPrint(key.Code) {
		return append(out, string(key.Code)...)
	}
	return nil
}

// namedKey maps the special (non-text) keys to their byte sequences. Enter,
// Tab, Esc, and Backspace resolve to a single C0/DEL byte; the navigation
// cluster to its CSI/SS3 sequence in the default (non-application) mode.
func namedKey(code rune) ([]byte, bool) {
	switch code {
	case tea.KeyEnter:
		return []byte{'\r'}, true
	case tea.KeyTab:
		return []byte{'\t'}, true
	case tea.KeyEscape:
		return []byte{0x1b}, true
	case tea.KeyBackspace:
		return []byte{0x7f}, true
	case tea.KeyUp:
		return []byte("\x1b[A"), true
	case tea.KeyDown:
		return []byte("\x1b[B"), true
	case tea.KeyRight:
		return []byte("\x1b[C"), true
	case tea.KeyLeft:
		return []byte("\x1b[D"), true
	case tea.KeyHome:
		return []byte("\x1b[H"), true
	case tea.KeyEnd:
		return []byte("\x1b[F"), true
	case tea.KeyInsert:
		return []byte("\x1b[2~"), true
	case tea.KeyDelete:
		return []byte("\x1b[3~"), true
	case tea.KeyPgUp:
		return []byte("\x1b[5~"), true
	case tea.KeyPgDown:
		return []byte("\x1b[6~"), true
	}
	return nil, false
}

// ctrlByte maps a base key held with Ctrl to its C0 control byte, following
// the standard terminal rule (letters to 0x01–0x1A, and the punctuation that
// completes the C0 range). It reports false for keys with no control byte, so
// the caller can fall back to the key's text.
func ctrlByte(code rune) (byte, bool) {
	switch {
	case code >= 'a' && code <= 'z':
		return byte(code-'a') + 1, true
	case code >= 'A' && code <= 'Z':
		return byte(code-'A') + 1, true
	}
	switch code {
	case ' ', '@':
		return 0x00, true // Ctrl-Space, Ctrl-@ -> NUL
	case '[':
		return 0x1b, true // Ctrl-[ -> ESC
	case '\\':
		return 0x1c, true
	case ']':
		return 0x1d, true
	case '^':
		return 0x1e, true
	case '_', '/':
		return 0x1f, true // Ctrl-_ and the common Ctrl-/
	case '?':
		return 0x7f, true
	}
	return 0, false
}
