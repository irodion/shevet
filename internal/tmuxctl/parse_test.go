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

// TestParser_FlowControlNotifications covers the pause-after forms (ADR-0008):
// %extended-output replaces %output once flow control is on, and %pause /
// %continue bracket a paused pane.
func TestParser_FlowControlNotifications(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want Event
	}{
		{
			name: "extended-output decodes like output",
			line: `%extended-output %1 0 : \033[31mred`,
			want: OutputEvent{PaneID: "%1", Data: []byte("\x1b[31mred")},
		},
		{
			name: "extended-output with nonzero age",
			line: `%extended-output %7 1234 : hello`,
			want: OutputEvent{PaneID: "%7", Data: []byte("hello")},
		},
		{
			name: "extended-output tolerates reserved fields",
			line: `%extended-output %2 5 someflag : data`,
			want: OutputEvent{PaneID: "%2", Data: []byte("data")},
		},
		{
			name: "extended-output value may contain a colon",
			line: `%extended-output %3 0 : ratio 3:1`,
			want: OutputEvent{PaneID: "%3", Data: []byte("ratio 3:1")},
		},
		{
			name: "pause",
			line: "%pause %4",
			want: PauseEvent{PaneID: "%4"},
		},
		{
			name: "continue",
			line: "%continue %4",
			want: ContinueEvent{PaneID: "%4"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &parser{}
			ev, done := p.feed(tc.line)
			if done != nil {
				t.Fatalf("feed(%q) returned a reply %+v, want an event", tc.line, done)
			}
			if !reflect.DeepEqual(ev, tc.want) {
				t.Errorf("feed(%q) = %+v, want %+v", tc.line, ev, tc.want)
			}
		})
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
