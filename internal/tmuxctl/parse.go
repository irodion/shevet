package tmuxctl

import (
	"strings"
)

// reply is one completed %begin/%end command reply.
type reply struct {
	lines []string
	isErr bool   // terminated by %error instead of %end
	seq   uint64 // stream position of the closing line; set by the reader
}

// parser turns the line stream of a tmux control-mode client into Events and
// replies. It is a pure state machine — no I/O — so control-mode transcripts
// can drive it directly in tests.
//
// Stream shape (tmux(1), CONTROL MODE): notifications are single lines
// starting with '%'; command replies are '%begin <ts> <num> <flags>' blocks
// closed by a matching '%end'/'%error'. tmux writes a reply as one unit, so
// everything between the delimiters — including lines that start with '%',
// e.g. pane ids in list-panes output — is reply body.
type parser struct {
	inReply  bool
	replyNum string
	body     []string
	isErr    bool
}

// topologyNotifications are the notifications after which the pane set, a
// pane's geometry, or a title may differ from the Server's registry.
var topologyNotifications = map[string]bool{
	"%window-add":            true,
	"%window-close":          true,
	"%unlinked-window-close": true,
	"%window-renamed":        true,
	"%layout-change":         true,
	"%window-pane-changed":   true,
	"%session-window-changed": true,
}

// feed consumes one line. It returns a non-nil Event for notifications and a
// non-nil reply when a %begin block completes; both are nil for lines inside
// a still-open reply.
func (p *parser) feed(line string) (Event, *reply) {
	if p.inReply {
		if num, isErr, ok := replyEnd(line); ok && num == p.replyNum {
			done := &reply{lines: p.body, isErr: isErr}
			p.inReply, p.body, p.isErr = false, nil, false
			return nil, done
		}
		p.body = append(p.body, line)
		return nil, nil
	}

	word, rest, _ := strings.Cut(line, " ")
	switch {
	case word == "%begin":
		p.inReply = true
		p.replyNum = field(rest, 1)
		return nil, nil
	case word == "%output":
		pane, data, _ := strings.Cut(rest, " ")
		return OutputEvent{PaneID: pane, Data: decodeOutput(data)}, nil
	case word == "%exit":
		return ExitEvent{Reason: rest}, nil
	case topologyNotifications[word]:
		return TopologyEvent{Notification: line}, nil
	default:
		return UnknownEvent{Line: line}, nil
	}
}

// replyEnd reports whether line closes a reply, and with which command
// number. Both '%end <ts> <num> <flags>' and '%error ...' close one.
func replyEnd(line string) (num string, isErr bool, ok bool) {
	word, rest, _ := strings.Cut(line, " ")
	if word != "%end" && word != "%error" {
		return "", false, false
	}
	return field(rest, 1), word == "%error", true
}

// field returns the i-th space-separated field of s, or "".
func field(s string, i int) string {
	f := strings.Fields(s)
	if i >= len(f) {
		return ""
	}
	return f[i]
}

// decodeOutput reverses the escaping tmux applies to %output data (vis(3)
// with octal and C-style encodings; valid UTF-8 passes through literally):
// '\\' for a backslash, three-digit octal '\NNN' for control bytes, and the
// C escapes for the bytes that have them. An unrecognized escape is kept
// verbatim rather than dropped.
func decodeOutput(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			out = append(out, s[i])
			continue
		}
		i++
		switch c := s[i]; {
		case c >= '0' && c <= '7':
			b := c - '0'
			for n := 0; n < 2 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7'; n++ {
				i++
				b = b<<3 | (s[i] - '0')
			}
			out = append(out, b)
		case c == '\\':
			out = append(out, '\\')
		case c == 'a':
			out = append(out, '\a')
		case c == 'b':
			out = append(out, '\b')
		case c == 'f':
			out = append(out, '\f')
		case c == 'n':
			out = append(out, '\n')
		case c == 'r':
			out = append(out, '\r')
		case c == 't':
			out = append(out, '\t')
		case c == 'v':
			out = append(out, '\v')
		default:
			out = append(out, '\\', c)
		}
	}
	return out
}
