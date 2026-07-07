// Package tmuxctl attaches to a tmux server in control mode (`tmux -C`) and
// exposes what the Server needs from it: the live notification stream
// (Events) and a request/reply command channel (Command).
//
// This is the process-control seam of the architecture: tmux owns PTYs and
// process persistence, Shevet owns interpretation (ARCHITECTURE.md §3.1) —
// the same mechanism iTerm2's tmux integration uses.
package tmuxctl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// maxLineSize bounds one control-mode line. %output lines carry a whole
// pane write escaped up to 4x, so this is generous rather than tight.
const maxLineSize = 4 * 1024 * 1024

// closeTimeout is how long Close waits for tmux to exit after stdin closes
// (EOF on stdin means detach) before killing the process.
const closeTimeout = 5 * time.Second

// Options configures a control-mode attachment.
type Options struct {
	// Socket is the tmux server's socket path (tmux -S). Empty uses the
	// default tmux server for the user.
	Socket string

	// Session is the tmux session to attach to, matched exactly. The
	// control client receives output and topology notifications for this
	// session's panes. Required.
	Session string
}

// Client is one control-mode attachment to a tmux server. Construct with
// Attach; always Close. Command is safe for concurrent use.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer

	events    chan Event
	done      chan struct{} // closed when the reader goroutine ends
	err       error         // set before done closes
	quit      chan struct{} // closed by Close: stop delivering events
	closeOnce sync.Once

	mu        sync.Mutex
	pending   []chan reply // FIFO: tmux answers commands in send order
	guardSeen bool         // the attach guard reply has been consumed
}

// Attach starts a control-mode tmux client attached to the session and
// verifies the attachment with a probe command, so a missing session or
// server fails here rather than surfacing later as a dead event stream.
//
// The process lives until Close or ctx cancellation.
func Attach(ctx context.Context, opts Options) (*Client, error) {
	if opts.Session == "" {
		return nil, errors.New("tmuxctl: session must not be empty")
	}

	args := []string{"-C"}
	if opts.Socket != "" {
		args = append(args, "-S", opts.Socket)
	}
	// '=' pins exact-name matching; bare names would prefix-match.
	args = append(args, "attach-session", "-t", "="+opts.Session)

	cmd := exec.CommandContext(ctx, "tmux", args...)
	// Scrub TMUX so attaching works identically whether or not the Server
	// itself runs inside a tmux pane (it does, under the respawn wrapper).
	cmd.Env = environWithout("TMUX=")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("tmuxctl: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("tmuxctl: stdout pipe: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tmuxctl: start tmux: %w", err)
	}

	c := &Client{
		cmd:    cmd,
		stdin:  stdin,
		stderr: stderr,
		events: make(chan Event, 256),
		done:   make(chan struct{}),
		quit:   make(chan struct{}),
	}
	go c.readLoop(stdout)

	// Probe: any reply proves the attachment is live. A failed attach
	// (no such session, no server) ends the stream instead, and Command
	// surfaces the process error with tmux's stderr.
	probeCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()
	if _, err := c.Command(probeCtx, "display-message", "-p", "ok"); err != nil {
		c.Close() //nolint:errcheck // already failing; process cleanup only
		return nil, fmt.Errorf("tmuxctl: attach to session %q: %w", opts.Session, err)
	}
	return c, nil
}

// Events returns the notification stream. The channel is closed when the
// control client ends (after an ExitEvent when tmux said why).
func (c *Client) Events() <-chan Event {
	return c.events
}

// Command sends one tmux command and returns its reply body. Arguments are
// quoted for tmux's command parser, so callers pass plain words — including
// format strings with spaces. tmux replying %error is returned as an error
// with the reply text.
func (c *Client) Command(ctx context.Context, args ...string) ([]string, error) {
	lines, _, err := c.CommandSeq(ctx, args...)
	return lines, err
}

// CommandSeq is Command plus the reply's control-stream position: any
// OutputEvent with a smaller Seq was already applied to tmux's screen when
// this command executed. That ordering is what lets a capture-pane snapshot
// and the live output stream be stitched together without duplication.
func (c *Client) CommandSeq(ctx context.Context, args ...string) ([]string, uint64, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteArg(a)
	}
	line := strings.Join(quoted, " ") + "\n"

	ch := make(chan reply, 1)
	// Enqueuing and writing must be atomic across callers: tmux matches
	// replies to commands purely by order.
	c.mu.Lock()
	c.pending = append(c.pending, ch)
	_, err := io.WriteString(c.stdin, line)
	c.mu.Unlock()
	if err != nil {
		return nil, 0, fmt.Errorf("tmuxctl: send command: %w", err)
	}

	select {
	case r := <-ch:
		if r.isErr {
			return nil, 0, fmt.Errorf("tmuxctl: tmux: %s", strings.Join(r.lines, " "))
		}
		return r.lines, r.seq, nil
	case <-c.done:
		return nil, 0, c.streamErr()
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

// Close detaches (EOF on stdin) and waits for tmux to exit, escalating to a
// kill after closeTimeout. It is safe to call more than once, and does not
// require the caller to keep draining Events.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.quit) })
	c.stdin.Close() //nolint:errcheck // EOF is the detach signal; already-closed is fine

	select {
	case <-c.done:
	case <-time.After(closeTimeout):
		if c.cmd.Process != nil {
			c.cmd.Process.Kill() //nolint:errcheck // escalation; Wait below reports state
		}
		<-c.done
	}
	return nil
}

// readLoop parses the control-mode stream, routing replies to their waiting
// commands and notifications to the events channel. It owns the process
// Wait: when it returns, the stream is over and every waiter is released.
func (c *Client) readLoop(stdout io.Reader) {
	parse := &parser{}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)

	var seq uint64
	for scanner.Scan() {
		seq++
		ev, done := parse.feed(scanner.Text())
		if done != nil {
			done.seq = seq
			c.completeReply(*done)
		}
		if out, ok := ev.(OutputEvent); ok {
			out.Seq = seq
			ev = out
		}
		if ev != nil {
			// A closed Client stops delivering instead of blocking on a
			// consumer that has already gone away.
			select {
			case c.events <- ev:
			case <-c.quit:
			}
		}
	}

	waitErr := c.cmd.Wait()
	c.err = c.exitError(scanner.Err(), waitErr)
	// Order matters: err must be visible before done releases the waiters
	// in Command and streamErr.
	close(c.done)
	close(c.events)
}

// completeReply hands a finished reply to the oldest waiting command.
//
// The very first reply block is never a command's: it is the guard block
// tmux emits for the attach command itself. It must be discarded by
// position, not by whether a waiter exists — a command sent quickly after
// attach can be pending before the guard arrives, and matching the guard to
// it would shift every reply to the wrong command from then on.
func (c *Client) completeReply(r reply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.guardSeen {
		c.guardSeen = true
		return
	}
	if len(c.pending) == 0 {
		return
	}
	ch := c.pending[0]
	c.pending = c.pending[1:]
	ch <- r
}

// streamErr describes why the stream ended, for commands it stranded.
// Stranded waiters are released by the done channel, never by closing their
// reply channels — a closed channel's zero value would read as success.
func (c *Client) streamErr() error {
	<-c.done
	return c.err
}

// exitError condenses scanner and process state into one error.
func (c *Client) exitError(scanErr, waitErr error) error {
	msg := strings.TrimSpace(c.stderr.String())
	switch {
	case scanErr != nil:
		return fmt.Errorf("tmuxctl: control stream: %w", scanErr)
	case waitErr != nil && msg != "":
		return fmt.Errorf("tmuxctl: tmux exited: %v: %s", waitErr, msg)
	case waitErr != nil:
		return fmt.Errorf("tmuxctl: tmux exited: %w", waitErr)
	case msg != "":
		return fmt.Errorf("tmuxctl: tmux exited: %s", msg)
	default:
		return errors.New("tmuxctl: control stream ended")
	}
}

// quoteArg wraps an argument for tmux's command-line parser, which splits on
// spaces and honors single quotes. Simple words pass through untouched so
// commands stay readable in logs and transcripts.
func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"\\;#{}$") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// environWithout returns the process environment minus entries with the
// given prefix.
func environWithout(prefix string) []string {
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}
