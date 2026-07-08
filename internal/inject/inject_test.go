package inject

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recorder is a fake Commander that captures every send-keys invocation and,
// optionally, fails the nth call — enough to assert both the exact command
// stream and error propagation without a real tmux.
type recorder struct {
	calls  [][]string
	failAt int // 1-based index of the call to fail; 0 never fails
	err    error
}

func (r *recorder) Command(_ context.Context, args ...string) ([]string, error) {
	r.calls = append(r.calls, args)
	if r.failAt != 0 && len(r.calls) == r.failAt {
		return nil, r.err
	}
	return nil, nil
}

const pane = "%1"

// lit and ctl build the expected argument slices for a literal run and a
// control run, keeping the table readable.
func lit(s string) []string { return []string{"send-keys", "-t", pane, "-l", "--", s} }
func ctl(hexes ...string) []string {
	return append([]string{"send-keys", "-t", pane, "-H"}, hexes...)
}

// TestKeys_ExhaustiveTable pins the injector's exact send-keys output for the
// input classes the review hardened: printable ASCII, multi-byte UTF-8 that
// must never touch -H, control bytes that must never touch -l, and the mixed
// sequences (arrow keys, "type then Enter") where a run boundary falls.
func TestKeys_ExhaustiveTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want [][]string
	}{
		{"empty", "", nil},
		{"printable ascii", "hello", [][]string{lit("hello")}},
		{"space is literal", " ", [][]string{lit(" ")}},
		{"enter", "\r", [][]string{ctl("0d")}},
		{"newline", "\n", [][]string{ctl("0a")}},
		{"tab", "\t", [][]string{ctl("09")}},
		{"escape", "\x1b", [][]string{ctl("1b")}},
		{"ctrl-c", "\x03", [][]string{ctl("03")}},
		{"nul", "\x00", [][]string{ctl("00")}},
		{"del", "\x7f", [][]string{ctl("7f")}},
		{"unit separator then space (0x1f/0x20 boundary)", "\x1f ", [][]string{ctl("1f"), lit(" ")}},

		// UTF-8 ≥ 0x80 is always literal — the double-encoding trap.
		{"latin-1 accent", "café", [][]string{lit("café")}},
		{"cyrillic", "я", [][]string{lit("я")}},
		{"cjk", "中", [][]string{lit("中")}},
		{"emoji zwj sequence", "👩‍🚀", [][]string{lit("👩‍🚀")}},
		{"mixed ascii and wide", "a你b", [][]string{lit("a你b")}},

		// Mixed sequences: the ordering and run boundaries are the contract.
		{"arrow up", "\x1b[A", [][]string{ctl("1b"), lit("[A")}},
		{"type then enter", "café\r", [][]string{lit("café"), ctl("0d")}},
		{"control in the middle", "ab\x03cd", [][]string{lit("ab"), ctl("03"), lit("cd")}},
		{"adjacent controls batch into one call", "\x03\x04", [][]string{ctl("03", "04")}},
		{"esc-bracket then two controls", "\x1b[\x00\x01", [][]string{ctl("1b"), lit("["), ctl("00", "01")}},

		// A leading dash must stay literal text, not a flag — hence `--`.
		{"leading dash", "-x", [][]string{lit("-x")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{}
			if err := Keys(context.Background(), r, pane, []byte(tc.in)); err != nil {
				t.Fatalf("Keys: %v", err)
			}
			assertCalls(t, r.calls, tc.want)
		})
	}
}

// TestKeys_EveryByteRoutedByClass is the truly exhaustive check: every one of
// the 256 byte values, sent alone, lands on the correct path and nowhere
// else — the two "never" invariants, proven over the whole byte space.
func TestKeys_EveryByteRoutedByClass(t *testing.T) {
	for b := 0; b < 256; b++ {
		r := &recorder{}
		if err := Keys(context.Background(), r, pane, []byte{byte(b)}); err != nil {
			t.Fatalf("byte %#02x: Keys: %v", b, err)
		}
		if len(r.calls) != 1 {
			t.Fatalf("byte %#02x: got %d commands, want 1", b, len(r.calls))
		}
		flag := r.calls[0][3] // send-keys -t %1 <flag> ...
		control := b < 0x20 || b == 0x7f
		switch {
		case control && flag != "-H":
			t.Errorf("control byte %#02x routed via %q, want -H", b, flag)
		case !control && flag != "-l":
			t.Errorf("non-control byte %#02x routed via %q, want -l", b, flag)
		}
	}
}

// TestKeys_ByteFidelity is a property test over a byte-diverse corpus:
// whatever runs the injector emits, decoding them back must reproduce the
// input exactly, no -H argument may carry a byte ≥ 0x80, and no literal run
// may carry a control byte. This is the encoder half of the end-to-end
// byte-identity the e2e harness proves against real tmux.
func TestKeys_ByteFidelity(t *testing.T) {
	corpus := []byte("plain\ttab\r\n\x1b[31mred\x1b[0m café 你好 👩‍🚀 back\\slash \x00\x01\x06\x7f -dash;{}$ end")

	r := &recorder{}
	if err := Keys(context.Background(), r, pane, corpus); err != nil {
		t.Fatalf("Keys: %v", err)
	}

	var got []byte
	for _, call := range r.calls {
		switch call[3] {
		case "-l":
			if literal := call[5]; !allLiteralSafe(literal) {
				t.Errorf("literal run %q contains a control byte (must go via -H)", literal)
			} else {
				got = append(got, literal...)
			}
		case "-H":
			for _, h := range call[4:] {
				raw, err := hex.DecodeString(h)
				if err != nil || len(raw) != 1 {
					t.Fatalf("malformed hex arg %q", h)
				}
				if raw[0] >= 0x80 {
					t.Errorf("byte %#02x injected via -H (must go via -l)", raw[0])
				}
				got = append(got, raw...)
			}
		default:
			t.Fatalf("unexpected send-keys flag %q", call[3])
		}
	}

	if string(got) != string(corpus) {
		t.Errorf("decoded injection differs from input:\n got  %q\n want %q", got, corpus)
	}
}

// TestKeys_StopsAndReportsOnCommandError: a failing send-keys aborts the
// injection at that point and surfaces the error, rather than pressing on
// with a corrupted, half-delivered sequence.
func TestKeys_StopsAndReportsOnCommandError(t *testing.T) {
	sentinel := errors.New("tmux gone")
	r := &recorder{failAt: 2, err: sentinel}

	// "a" (literal) then Enter (control): two commands; the second fails.
	err := Keys(context.Background(), r, pane, []byte("a\r"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
	if len(r.calls) != 2 {
		t.Errorf("issued %d commands, want it to stop at the failing 2nd", len(r.calls))
	}
	if !strings.Contains(err.Error(), pane) {
		t.Errorf("error %q does not name the pane", err)
	}
}

func allLiteralSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if isControl(s[i]) {
			return false
		}
	}
	return true
}

func assertCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d commands, want %d\n got  %s\n want %s",
			len(got), len(want), fmtCalls(got), fmtCalls(want))
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func fmtCalls(calls [][]string) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = fmt.Sprintf("[%s]", strings.Join(c, " "))
	}
	return strings.Join(parts, " ")
}
