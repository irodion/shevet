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
	"math"
	"os"
	"os/exec"
	"strconv"
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

	// PauseAfter, when > 0, sets tmux's pause-after flow-control flag to this
	// many seconds (rounded up, minimum 1): output that tmux has buffered for
	// the client longer than this pauses the pane rather than growing without
	// bound (ADR-0008). It is the whole-stream backstop; the Server also
	// pauses individual saturated panes itself. Requires tmux ≥ 3.2.
	PauseAfter time.Duration
}

// minTmuxMajor and minTmuxMinor are the floor Shevet supports: control-mode
// pause flow control (pause-after, %pause/%continue, %extended-output) shipped
// in tmux 3.2 (ADR-0008).
const (
	minTmuxMajor = 3
	minTmuxMinor = 2
)

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
	pending   []pendingReply // FIFO: tmux answers commands in send order
	guardSeen bool           // the attach guard reply has been consumed
	guardErr  string         // the guard's text when it was an %error: why the attach failed

	// evQueue decouples event delivery from reply routing: the reader
	// enqueues without ever blocking, a pump goroutine feeds Events().
	// This is load-bearing, not a buffer tweak — a consumer is allowed to
	// stop draining Events while it waits on Command (the watcher does
	// exactly that during reconcile), and if the reader could block on a
	// full events channel it would never reach the reply line that
	// consumer is waiting for: a deadlock. The queue is unbounded; it only
	// grows during those reply waits, which are single tmux round-trips.
	evMu     sync.Mutex
	evQueue  []Event
	evEnded  bool          // reader finished; pump closes events once drained
	evNotify chan struct{} // cap 1: kick the pump
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
	if err := checkVersion(ctx); err != nil {
		return nil, err
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
		cmd:      cmd,
		stdin:    stdin,
		stderr:   stderr,
		events:   make(chan Event),
		done:     make(chan struct{}),
		quit:     make(chan struct{}),
		evNotify: make(chan struct{}, 1),
	}
	go c.readLoop(stdout)
	go c.pumpEvents()

	// Probe: any reply proves the attachment is live. A failed attach
	// (no such session, no server) ends the stream instead, and Command
	// surfaces the process error with tmux's stderr.
	probeCtx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()
	if _, err := c.Command(probeCtx, "display-message", "-p", "ok"); err != nil {
		c.Close() //nolint:errcheck // already failing; process cleanup only
		return nil, fmt.Errorf("tmuxctl: attach to session %q: %w", opts.Session, err)
	}

	// Enable flow control on this control client. Setting it here (not via the
	// attach args) keeps it one well-defined command after the attach is
	// proven live, and switches pane output to the %extended-output form.
	if opts.PauseAfter > 0 {
		// Round up to whole seconds; the enclosing PauseAfter > 0 guard makes
		// the result at least 1.
		secs := int(math.Ceil(opts.PauseAfter.Seconds()))
		if _, err := c.Command(probeCtx, "refresh-client", "-f", fmt.Sprintf("pause-after=%d", secs)); err != nil {
			c.Close() //nolint:errcheck // already failing; process cleanup only
			return nil, fmt.Errorf("tmuxctl: enable pause-after flow control: %w", err)
		}
	}
	return c, nil
}

// checkVersion fails when the tmux binary is older than the control-mode
// flow-control floor (ADR-0008). tmux -V reports the binary version and needs
// no socket; a version string Shevet cannot parse (a development build such as
// "tmux next-3.5" or "tmux master") is allowed through rather than blocking an
// upgrade.
func checkVersion(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "tmux", "-V").Output()
	if err != nil {
		return fmt.Errorf("tmuxctl: determine tmux version: %w", err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "tmux "))
	major, minor, ok := parseVersion(version)
	if !ok {
		return nil
	}
	if belowFloor(major, minor) {
		return fmt.Errorf("tmuxctl: tmux %s is too old: Shevet needs tmux %d.%d or newer for "+
			"control-mode flow control (pause-after); please upgrade tmux",
			version, minTmuxMajor, minTmuxMinor)
	}
	return nil
}

// belowFloor reports whether a major.minor version is older than the supported
// tmux floor.
func belowFloor(major, minor int) bool {
	return major < minTmuxMajor || (major == minTmuxMajor && minor < minTmuxMinor)
}

// parseVersion extracts the leading major.minor from a tmux version string,
// tolerating a "next-" prefix (development builds) and a trailing letter
// (OpenBSD portable releases, e.g. "3.2a"). It reports ok=false when no
// major.minor is present, which the caller treats as "assume new enough".
func parseVersion(v string) (major, minor int, ok bool) {
	v = strings.TrimPrefix(v, "next-")
	dot := strings.IndexByte(v, '.')
	if dot <= 0 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(v[:dot])
	if err != nil {
		return 0, 0, false
	}
	rest := v[dot+1:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(rest[:end])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
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

// pendingReply is a waiting command's reply channel. cont marks a slot as a
// continuation of the previous slot's command within one atomic ';'-sequence:
// tmux aborts such a sequence after the first error, so on an errored reply
// completeReply purges the trailing cont slots (they will never be answered)
// to keep later commands matched. A standalone command's slot has cont false.
type pendingReply struct {
	ch   chan reply
	cont bool
}

// CommandSeq is Command plus the reply's control-stream position: any
// OutputEvent with a smaller Seq was already applied to tmux's screen when
// this command executed. That ordering is what lets a capture-pane snapshot
// and the live output stream be stitched together without duplication.
func (c *Client) CommandSeq(ctx context.Context, args ...string) ([]string, uint64, error) {
	replies, seq, err := c.CommandsSeq(ctx, args)
	if err != nil {
		return nil, 0, err
	}
	return replies[0], seq, nil
}

// CommandsSeq runs several tmux commands as one atomic control-mode line —
// joined by tmux's `;` command separator — and returns exactly one reply body
// per command, in order, plus the stream position of the first reply. tmux
// executes a `;`-joined sequence back-to-back with no %output interleaved
// between the replies, so every reply pins to a single stream position: a
// capture-pane snapshot and a display-message read in the same sequence observe
// the same screen (verified against tmux 3.7b). Sending the commands separately
// cannot promise that — output can land between them — which is the seed skew
// that shifts a reconnected pane's screen (issue #53).
//
// tmux runs a sequence up to the first error and stops: the erroring command
// emits its block (surfaced here as the error), and every command after it is
// skipped with no block. completeReply purges those skipped slots so later
// commands stay matched. Both this and the no-interleave guarantee are
// long-standing tmux command-queue behaviors, relied on across the ≥ 3.2 floor
// (ADR-0008) and verified on 3.7b.
func (c *Client) CommandsSeq(ctx context.Context, cmds ...[]string) ([][]string, uint64, error) {
	if len(cmds) == 0 {
		return nil, 0, nil
	}
	parts := make([]string, len(cmds))
	pend := make([]pendingReply, len(cmds))
	chans := make([]chan reply, len(cmds))
	for i, args := range cmds {
		parts[i] = quoteCommand(args)
		chans[i] = make(chan reply, 1)
		pend[i] = pendingReply{ch: chans[i], cont: i > 0}
	}
	line := strings.Join(parts, " ; ") + "\n"

	// Enqueue every reply channel and write the one line atomically: tmux
	// matches replies to commands purely by order, so the blocks for this
	// sequence must claim consecutive slots with no other command's write
	// interleaved.
	c.mu.Lock()
	c.pending = append(c.pending, pend...)
	_, err := io.WriteString(c.stdin, line)
	c.mu.Unlock()
	if err != nil {
		return nil, 0, fmt.Errorf("tmuxctl: send commands: %w", err)
	}

	replies := make([][]string, len(cmds))
	var firstSeq uint64
	for i, ch := range chans {
		select {
		case r := <-ch:
			if r.isErr {
				return nil, 0, fmt.Errorf("tmuxctl: tmux: %s", strings.Join(r.lines, " "))
			}
			if i == 0 {
				firstSeq = r.seq
			}
			replies[i] = r.lines
		case <-c.done:
			return nil, 0, c.streamErr()
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	return replies, firstSeq, nil
}

// quoteCommand quotes one command's args for tmux's parser and joins them into
// a single command string (as it appears within a control-mode line).
func quoteCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteArg(a)
	}
	return strings.Join(quoted, " ")
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
			c.enqueueEvent(ev)
		}
	}

	waitErr := c.cmd.Wait()
	c.err = c.exitError(scanner.Err(), waitErr)
	// Order matters: err must be visible before done releases the waiters
	// in Command and streamErr.
	close(c.done)
	c.endEvents()
}

// enqueueEvent hands a notification to the pump; it never blocks (see the
// evQueue field comment for why that is a hard requirement).
func (c *Client) enqueueEvent(ev Event) {
	c.evMu.Lock()
	c.evQueue = append(c.evQueue, ev)
	c.evMu.Unlock()
	select {
	case c.evNotify <- struct{}{}:
	default:
	}
}

// endEvents tells the pump the stream is over; it closes Events() once the
// queue is drained.
func (c *Client) endEvents() {
	c.evMu.Lock()
	c.evEnded = true
	c.evMu.Unlock()
	select {
	case c.evNotify <- struct{}{}:
	default:
	}
}

// pumpEvents delivers queued notifications to the Events channel, in stream
// order. A closed Client discards instead of delivering, so teardown never
// depends on a consumer still draining.
func (c *Client) pumpEvents() {
	for {
		c.evMu.Lock()
		queue := c.evQueue
		c.evQueue = nil
		ended := c.evEnded
		c.evMu.Unlock()

		for _, ev := range queue {
			select {
			case c.events <- ev:
			case <-c.quit:
			}
		}
		if len(queue) > 0 {
			continue // the queue may have refilled; re-check before sleeping
		}
		if ended {
			close(c.events)
			return
		}
		<-c.evNotify
	}
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
		// A failed attach (no server, no such session) reports its reason
		// as an %error guard on stdout — nothing goes to stderr. Keep the
		// text so the exit error can say why instead of "exit status 1".
		if r.isErr {
			c.guardErr = strings.Join(r.lines, " ")
		}
		return
	}
	if len(c.pending) == 0 {
		return
	}
	p := c.pending[0]
	c.pending = c.pending[1:]
	p.ch <- r

	// tmux aborts a ';'-sequence after an error, so the commands after the
	// failing one get no reply block. Purge their slots — the trailing
	// continuations of this sequence — here in the reader thread, before the
	// next block is routed, or the following command would be answered from a
	// stranded slot. Done under the same lock hold, so no block can slip in.
	if r.isErr {
		for len(c.pending) > 0 && c.pending[0].cont {
			stranded := c.pending[0].ch
			c.pending = c.pending[1:]
			stranded <- reply{isErr: true, lines: []string{"tmux aborted the command sequence"}}
		}
	}
}

// streamErr describes why the stream ended, for commands it stranded.
// Stranded waiters are released by the done channel, never by closing their
// reply channels — a closed channel's zero value would read as success.
func (c *Client) streamErr() error {
	<-c.done
	return c.err
}

// exitError condenses scanner, guard, and process state into one error.
func (c *Client) exitError(scanErr, waitErr error) error {
	msg := strings.TrimSpace(c.stderr.String())
	switch {
	case scanErr != nil:
		return fmt.Errorf("tmuxctl: control stream: %w", scanErr)
	case c.guardErr != "":
		return fmt.Errorf("tmuxctl: tmux: %s", c.guardErr)
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
//
// '%' is in the quote set because the control-mode command parser treats a
// bare leading '%' as its own token type (e.g. %if): an unquoted flow-control
// target like `refresh-client -A %3:pause` is a parse error, and single
// quotes are what make it a plain argument.
func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"\\;#{}$%") {
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
