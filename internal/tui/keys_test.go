package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestEncodeKey covers the input-event table for the Client's key encoder:
// text (ASCII, shifted, and multi-byte UTF-8), the C0-named keys, Ctrl
// chords, the navigation cluster, and the Alt/Meta prefix — the keys an
// interactive shell or agent is driven with.
func TestEncodeKey(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		// Printable text, verbatim.
		{"letter", tea.KeyPressMsg{Code: 'a', Text: "a"}, "a"},
		{"shifted letter", tea.KeyPressMsg{Code: 'a', ShiftedCode: 'A', Text: "A", Mod: tea.ModShift}, "A"},
		{"digit", tea.KeyPressMsg{Code: '7', Text: "7"}, "7"},
		{"space", tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}, " "},
		{"latin-1 accent", tea.KeyPressMsg{Code: 'é', Text: "é"}, "é"},
		{"cyrillic", tea.KeyPressMsg{Code: 'я', Text: "я"}, "я"},
		{"cjk", tea.KeyPressMsg{Code: '中', Text: "中"}, "中"},
		{"emoji", tea.KeyPressMsg{Text: "👍"}, "👍"},

		// C0-named keys.
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyPressMsg{Code: tea.KeyTab}, "\t"},
		{"escape", tea.KeyPressMsg{Code: tea.KeyEscape}, "\x1b"},
		{"backspace", tea.KeyPressMsg{Code: tea.KeyBackspace}, "\x7f"},

		// Ctrl chords -> C0 control bytes.
		{"ctrl-c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, "\x03"},
		{"ctrl-a", tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}, "\x01"},
		{"ctrl-c with text still yields 0x03", tea.KeyPressMsg{Code: 'c', Text: "c", Mod: tea.ModCtrl}, "\x03"},
		{"ctrl-space is NUL", tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl}, "\x00"},
		{"ctrl-open-bracket is ESC", tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl}, "\x1b"},
		{"ctrl-backslash", tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl}, "\x1c"},

		// Navigation cluster.
		{"up", tea.KeyPressMsg{Code: tea.KeyUp}, "\x1b[A"},
		{"down", tea.KeyPressMsg{Code: tea.KeyDown}, "\x1b[B"},
		{"right", tea.KeyPressMsg{Code: tea.KeyRight}, "\x1b[C"},
		{"left", tea.KeyPressMsg{Code: tea.KeyLeft}, "\x1b[D"},
		{"home", tea.KeyPressMsg{Code: tea.KeyHome}, "\x1b[H"},
		{"end", tea.KeyPressMsg{Code: tea.KeyEnd}, "\x1b[F"},
		{"insert", tea.KeyPressMsg{Code: tea.KeyInsert}, "\x1b[2~"},
		{"delete", tea.KeyPressMsg{Code: tea.KeyDelete}, "\x1b[3~"},
		{"page up", tea.KeyPressMsg{Code: tea.KeyPgUp}, "\x1b[5~"},
		{"page down", tea.KeyPressMsg{Code: tea.KeyPgDown}, "\x1b[6~"},

		// Alt/Meta prefixes the base encoding with ESC.
		{"alt-a", tea.KeyPressMsg{Code: 'a', Text: "a", Mod: tea.ModAlt}, "\x1ba"},
		{"alt-enter", tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, "\x1b\r"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeKey(tc.key)
			if string(got) != tc.want {
				t.Errorf("encodeKey(%s) = %q, want %q", tea.Key(tc.key).Keystroke(), got, tc.want)
			}
		})
	}
}

// TestEncodeKey_NoBytes: keys that carry no input (a bare modifier press, an
// unmapped function key) encode to nothing, so the Client forwards nothing.
func TestEncodeKey_NoBytes(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: tea.KeyF1},
		{Code: tea.KeyLeftShift},
		{},
	} {
		if got := encodeKey(key); got != nil {
			t.Errorf("encodeKey(%s) = %q, want nil", tea.Key(key).Keystroke(), got)
		}
	}
}
