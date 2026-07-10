package tmuxctl_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/testutil"
	"github.com/irodion/shevet/internal/tmuxctl"
	"github.com/irodion/shevet/internal/tmuxtest"
)

// These are the against-real-tmux tests: the parser is covered by transcript
// fixtures, so what's verified here is the contract with tmux itself —
// attachment, reply matching, and byte-exact %output decoding.

func attach(t *testing.T, tm *tmuxtest.Tmux) *tmuxctl.Client {
	t.Helper()
	c, err := tmuxctl.Attach(context.Background(), tmuxctl.Options{
		Socket:  tm.Socket(),
		Session: "holder",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck // best-effort teardown
	return c
}

func TestAttach_FailsForMissingSession(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)

	_, err := tmuxctl.Attach(context.Background(), tmuxctl.Options{
		Socket:  tm.Socket(),
		Session: "no-such-session",
	})
	if err == nil {
		t.Fatal("Attach to a missing session succeeded, want error")
	}
	// tmux states the reason in an %error guard block on stdout; the error
	// must relay it, not just "exit status 1".
	if !strings.Contains(err.Error(), "no-such-session") {
		t.Errorf("error %q does not relay tmux's reason (the session name)", err)
	}
}

func TestAttach_FailsWithoutServer(t *testing.T) {
	t.Parallel()
	tmuxtest.Start(t) // skips when tmux is not installed

	_, err := tmuxctl.Attach(context.Background(), tmuxctl.Options{
		Socket:  filepath.Join(testutil.ShortDir(t), "no-server.sock"),
		Session: "holder",
	})
	if err == nil {
		t.Fatal("Attach with no tmux server succeeded, want error")
	}
}

// TestOutput_ByteFidelityUnderFlowControl is TestOutput_ByteFidelity with
// pause-after set: once flow control is on, tmux delivers pane output as
// %extended-output instead of %output (ADR-0008), and the decoded bytes must
// be byte-identical — the parser handling both forms is load-bearing.
func TestOutput_ByteFidelityUnderFlowControl(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c, err := tmuxctl.Attach(context.Background(), tmuxctl.Options{
		Socket:     tm.Socket(),
		Session:    "holder",
		PauseAfter: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck // best-effort teardown

	assertByteFidelity(t, tm, c)
}

func TestCommand_RepliesAndQuoting(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	// A format argument with spaces exercises argument quoting.
	lines, err := c.Command(ctx, "display-message", "-p", "-t", "holder", "#{session_name} is #{session_attached}")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "holder is ") {
		t.Errorf("reply = %q, want one line starting with %q", lines, "holder is ")
	}

	if _, err := c.Command(ctx, "frobnicate-hard"); err == nil {
		t.Error("unknown command succeeded, want error-reply surfaced as error")
	}
}

func TestCommandsSeq_RepliesInOrder(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	replies, _, err := c.CommandsSeq(ctx,
		[]string{"display-message", "-p", "first"},
		[]string{"display-message", "-p", "second"},
	)
	if err != nil {
		t.Fatalf("CommandsSeq: %v", err)
	}
	if len(replies) != 2 || len(replies[0]) != 1 || replies[0][0] != "first" || len(replies[1]) != 1 || replies[1][0] != "second" {
		t.Errorf("replies = %q, want [[first] [second]]", replies)
	}
}

// TestCommandsSeq_ErrorKeepsRepliesMatched pins the invariant the combined
// command relies on: tmux answers one reply block per command even when one
// errors, so a following command is never handed the wrong reply. Without it,
// an errored sequence would leave a pending slot and desync the whole stream.
func TestCommandsSeq_ErrorKeepsRepliesMatched(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	// First command targets a nonexistent pane and errors mid-sequence.
	if _, _, err := c.CommandsSeq(ctx,
		[]string{"capture-pane", "-p", "-t", "%99999"},
		[]string{"display-message", "-p", "unreached"},
	); err == nil {
		t.Fatal("a bad pane in the sequence did not surface an error")
	}

	// The reply stream must still be matched: a plain command gets its own
	// reply, not a stranded block from the errored sequence.
	lines, err := c.Command(ctx, "display-message", "-p", "still-matched")
	if err != nil {
		t.Fatalf("Command after errored sequence: %v", err)
	}
	if len(lines) != 1 || lines[0] != "still-matched" {
		t.Errorf("reply after errored sequence = %q, want [still-matched] (stream desynced)", lines)
	}
}

// TestCommandsSeq_AtomicSnapshotUnderFlood is the regression guard for issue
// #53: a `;`-joined sequence observes one stream position. Under a continuous
// flood, two #{history_size} reads in one CommandsSeq must be equal — no
// %output landed between them — even as the size climbs across iterations.
// Separately-sent commands would disagree, which is the seed skew this fixes.
//
// It runs both without flow control (plain %output) and with pause-after (the
// %extended-output form the real Server attaches with), since the seed runs
// under flow control and the atomicity must hold for that output form too.
func TestCommandsSeq_AtomicSnapshotUnderFlood(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		pauseAfter time.Duration
	}{
		{"plain output", 0},
		{"flow control", 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tm := tmuxtest.Start(t)
			c, err := tmuxctl.Attach(context.Background(), tmuxctl.Options{
				Socket:     tm.Socket(),
				Session:    "holder",
				PauseAfter: tc.pauseAfter,
			})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			t.Cleanup(func() { c.Close() }) //nolint:errcheck // best-effort teardown
			assertAtomicSnapshotUnderFlood(t, tm, c)
		})
	}
}

func assertAtomicSnapshotUnderFlood(t *testing.T, tm *tmuxtest.Tmux, c *tmuxctl.Client) {
	t.Helper()

	// A never-ending flood keeps output in flight for every iteration; drain
	// the notification stream so the queue stays bounded while we probe.
	pane := tm.NewWindow(t, "flood", "while true; do seq 1 100000; done")
	tm.WaitForContent(t, pane, "1")
	go func() {
		for range c.Events() { //nolint:revive // discard: this test only probes via replies
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	histRead := []string{"display-message", "-p", "-t", pane, "#{history_size}"}
	var prev int
	grew := false
	for i := 0; i < 200; i++ {
		replies, _, err := c.CommandsSeq(ctx, histRead, histRead)
		if err != nil {
			t.Fatalf("iteration %d: CommandsSeq: %v", i, err)
		}
		a, b := atoiReply(t, replies[0]), atoiReply(t, replies[1])
		if a != b {
			t.Fatalf("iteration %d: history_size not atomic: %d then %d — output interleaved between the sequence's replies", i, a, b)
		}
		if a > prev {
			grew = true
		}
		prev = a
	}
	if !grew {
		t.Fatal("history_size never grew; the flood did not exercise interleaving, so atomicity was not tested")
	}
}

func atoiReply(t *testing.T, lines []string) int {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("want one reply line, got %q", lines)
	}
	n, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		t.Fatalf("reply %q is not a number: %v", lines[0], err)
	}
	return n
}

func TestCommand_ConcurrentCallersGetTheirOwnReplies(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("reply-%d", i)
			lines, err := c.Command(ctx, "display-message", "-p", want)
			if err != nil {
				errs <- fmt.Errorf("command %d: %w", i, err)
				return
			}
			if len(lines) != 1 || lines[0] != want {
				errs <- fmt.Errorf("command %d got %q, want %q", i, lines, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestCommand_SucceedsWhileEventsUndrained pins the no-deadlock contract: a
// consumer may stop draining Events while it waits on a Command (the
// Server's watcher does, during every reconcile), so replies must get
// through even when a pane floods thousands of output notifications that
// nobody is consuming.
func TestCommand_SucceedsWhileEventsUndrained(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	// Nobody drains c.Events(); the flood must pile up harmlessly. ~2MB
	// of output arrives as hundreds of %output notifications.
	pane := tm.NewWindow(t, "flood", "seq 1 300000; sleep 86400")
	tm.WaitForContent(t, pane, "300000")

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	for i := 0; i < 20; i++ {
		if _, err := c.Command(ctx, "display-message", "-p", "ok"); err != nil {
			t.Fatalf("Command %d while events undrained: %v", i, err)
		}
	}

	// Keep the scenario honest: it must involve more queued events than any
	// fixed channel buffer could hide (the deadlock this test pins was a
	// reader blocked on a full 256-slot channel).
	events := 0
	deadline := time.After(testutil.WaitTimeout)
	for events <= 256 {
		select {
		case _, ok := <-c.Events():
			if !ok {
				t.Fatalf("event stream ended after only %d events", events)
			}
			events++
		case <-deadline:
			t.Fatalf("only %d events arrived; the flood no longer exercises the queue", events)
		}
	}
}

// assertByteFidelity cats a byte corpus into a pane on tm and checks that the
// bytes come out of c's event stream byte-identical — escapes, UTF-8, control
// characters, backslashes and all. The corpus is written newline-only and the
// expectation maps NL to CRNL, because the pane's pty has output
// post-processing (ONLCR) on, as any real Agent's pty does.
func assertByteFidelity(t *testing.T, tm *tmuxtest.Tmux, c *tmuxctl.Client) {
	t.Helper()
	corpus := "plain\ntab\there\n\x1b[31mred\x1b[0m\x1b[1;5H\nutf8 你好 café 👩‍🚀\nback\\slash\ncontrol \x01\x06\x7f end\n"
	want := []byte(strings.ReplaceAll(corpus, "\n", "\r\n"))

	path := filepath.Join(testutil.ShortDir(t), "corpus.bin")
	if err := os.WriteFile(path, []byte(corpus), 0o600); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	pane := tm.NewWindow(t, "corpus", "cat "+tmuxtest.ShellQuote(path)+"; sleep 86400")

	var got []byte
	deadline := time.After(testutil.WaitTimeout)
	for len(got) < len(want) {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("event stream ended early; got %d/%d bytes: %q", len(got), len(want), got)
			}
			if out, isOut := ev.(tmuxctl.OutputEvent); isOut && out.PaneID == pane {
				got = append(got, out.Data...)
			}
		case <-deadline:
			t.Fatalf("timed out; got %d/%d bytes: %q", len(got), len(want), got)
		}
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output bytes differ:\n got  %q\n want %q", got, want)
	}
}

// TestOutput_ByteFidelity is the decoder's ground truth against %output.
func TestOutput_ByteFidelity(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	assertByteFidelity(t, tm, attach(t, tm))
}

func TestTopologyEvents_OnWindowChanges(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	tm.NewWindow(t, "newborn", "sleep 86400")

	deadline := time.After(testutil.WaitTimeout)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatal("event stream ended before a topology event arrived")
			}
			if _, isTopo := ev.(tmuxctl.TopologyEvent); isTopo {
				return
			}
		case <-deadline:
			t.Fatal("no topology event after new-window")
		}
	}
}

func TestClose_EndsEventStream(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.After(testutil.WaitTimeout)
	for {
		select {
		case _, ok := <-c.Events():
			if !ok {
				return // closed, as promised
			}
		case <-deadline:
			t.Fatal("event stream still open after Close")
		}
	}
}
