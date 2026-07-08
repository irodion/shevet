package tmuxctl

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestParser_Transcript drives the parser with a control-mode session
// transcript: the attach guard reply, notifications, a command reply whose
// body lines start with '%' (list-panes output does), and the exit.
func TestParser_Transcript(t *testing.T) {
	transcript := strings.TrimSpace(`
%begin 1622 0 0
%end 1622 0 0
%session-changed $0 holder
%window-add @1
%output %1 hello
%layout-change @1 b25d,80x24,0,0,1 b25d,80x24,0,0,1 *
%begin 1623 1 1
%1 80 24 zsh
%2 80 24 top
%end 1623 1 1
%window-close @1
%exit
`)

	var events []Event
	var replies []reply
	p := &parser{}
	for _, line := range strings.Split(transcript, "\n") {
		ev, done := p.feed(line)
		if ev != nil {
			events = append(events, ev)
		}
		if done != nil {
			replies = append(replies, *done)
		}
	}

	wantEvents := []Event{
		UnknownEvent{Line: "%session-changed $0 holder"},
		TopologyEvent{Notification: "%window-add @1"},
		OutputEvent{PaneID: "%1", Data: []byte("hello")},
		TopologyEvent{Notification: "%layout-change @1 b25d,80x24,0,0,1 b25d,80x24,0,0,1 *"},
		TopologyEvent{Notification: "%window-close @1"},
		ExitEvent{},
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("events:\n got  %+v\n want %+v", events, wantEvents)
	}

	wantReplies := []reply{
		{lines: nil},
		{lines: []string{"%1 80 24 zsh", "%2 80 24 top"}},
	}
	if !reflect.DeepEqual(replies, wantReplies) {
		t.Errorf("replies:\n got  %+v\n want %+v", replies, wantReplies)
	}
}

func TestParser_ErrorReply(t *testing.T) {
	p := &parser{}
	p.feed("%begin 1 42 1")
	p.feed("unknown command: frobnicate")
	_, done := p.feed("%error 1 42 1")

	if done == nil || !done.isErr {
		t.Fatalf("reply = %+v, want an error reply", done)
	}
	if want := []string{"unknown command: frobnicate"}; !reflect.DeepEqual(done.lines, want) {
		t.Errorf("reply lines = %q, want %q", done.lines, want)
	}
}

func TestParser_EndWithWrongNumberStaysInReply(t *testing.T) {
	// A reply body could theoretically contain a line resembling a
	// delimiter; only the matching command number closes the block.
	p := &parser{}
	p.feed("%begin 1 7 1")
	if _, done := p.feed("%end 1 99 1"); done != nil {
		t.Fatal("mismatched end-marker closed the reply")
	}
	_, done := p.feed("%end 1 7 1")
	if done == nil {
		t.Fatal("matching end-marker did not close the reply")
	}
	if want := []string{"%end 1 99 1"}; !reflect.DeepEqual(done.lines, want) {
		t.Errorf("reply lines = %q, want %q", done.lines, want)
	}
}

func TestDecodeOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []byte
	}{
		{"plain ascii", "hello", []byte("hello")},
		{"octal control bytes", `line\015\012tab\011`, []byte("line\r\ntab\t")},
		{"escape sequence", `\033[31mred`, []byte("\x1b[31mred")},
		{"doubled backslash", `a\\b`, []byte(`a\b`)},
		{"utf8 passes through", "héllo 你好 👩‍🚀", []byte("héllo 你好 👩‍🚀")},
		{"c-style escapes", `\a\b\f\n\r\t\v`, []byte("\a\b\f\n\r\t\v")},
		{"short octal at end", `x\0`, []byte{'x', 0}},
		{"high byte octal", `\177\200`, []byte{0x7f, 0x80}},
		{"unknown escape kept verbatim", `a\qb`, []byte(`a\qb`)},
		{"trailing lone backslash kept", `a\`, []byte(`a\`)},
		{"empty", "", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeOutput(tc.in); !bytes.Equal(got, tc.want) {
				t.Errorf("decodeOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
