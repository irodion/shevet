package tmuxctl_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// TestOutput_ByteFidelity is the decoder's ground truth: bytes catted into a
// pane must come out of the event stream byte-identical — escapes, UTF-8,
// control characters, backslashes and all. The corpus is written newline-only
// and the expectation maps NL to CRNL, because the pane's pty has output
// post-processing (ONLCR) on, as any real Agent's pty does.
func TestOutput_ByteFidelity(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)
	c := attach(t, tm)

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
