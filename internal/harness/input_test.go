package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/scriptedagent"
	"github.com/irodion/shevet/internal/testutil"
)

// waitForPaneInHerd blocks until the Server's reconcile has admitted the pane,
// which is the precondition for injecting into it — input to a pane the Server
// isn't tracking is dropped by design.
func waitForPaneInHerd(t *testing.T, c *client.Client, pane string) {
	t.Helper()
	testutil.Eventually(t, "pane "+pane+" to enter the Herd", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
		defer cancel()
		panes, err := c.ListPanes(ctx)
		if err != nil {
			return false, err.Error()
		}
		for _, p := range panes {
			if p.ID == pane {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%v", panes)
	})
}

// sendInput opens a Control Input stream, forwards data to the pane, and
// closes it, returning the Server's delivery summary.
func sendInput(t *testing.T, c *client.Client, pane string, data []byte) client.InputSummary {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()

	in, err := c.SendInput(ctx)
	if err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if err := in.SendKeys(pane, data); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	sum, err := in.Close()
	if err != nil {
		t.Fatalf("close input stream: %v", err)
	}
	return sum
}

// TestInput_ByteDiverseCorpusArrivesIntact is the acceptance test of issue #9:
// a Client sends a byte-diverse corpus and a scripted agent, capturing its
// stdin in raw mode, receives it byte-identically — printable ASCII, control
// bytes (NUL and Ctrl-C included), escape sequences, and non-ASCII text
// (Cyrillic, CJK, emoji with a ZWJ). This exercises the whole input path:
// Control Input stream -> injector (send-keys -l / -H by byte class) ->
// tmux -> the pane's pty.
func TestInput_ByteDiverseCorpusArrivesIntact(t *testing.T) {
	t.Parallel()
	h := Start(t)

	corpus := []byte("ascii 123 punctuation!?;{}$-\t" + // printable + Tab + tmux metacharacters
		"\r" + // Enter (CR must not be translated to LF)
		"\x1b[A\x1b[D" + // arrow up / left (escape sequences)
		"\x00\x01\x03\x1b\x7f" + // NUL, SOH, Ctrl-C, ESC, DEL
		"Cyrillic Привет " + // ≥0x80: must ride the literal path
		"CJK 你好世界 " +
		"emoji 👩‍🚀🎉") // includes a zero-width joiner

	out := filepath.Join(testutil.ShortDir(t), "corpus.bin")
	pane := h.StartAgent(t, "raw", fmt.Sprintf("read-raw %d %s", len(corpus), out))
	waitForPaneInHerd(t, h.Client, pane)

	// The agent prints Ready only once its pty is in raw mode; waiting for it
	// on the rendered screen guarantees the bytes we inject arrive un-cooked.
	h.Tmux.WaitForContent(t, pane, scriptedagent.ReadRawReady)

	sum := sendInput(t, h.Client, pane, corpus)
	if sum.Events != 1 || sum.Bytes != uint64(len(corpus)) {
		t.Errorf("summary = %+v, want events=1 bytes=%d", sum, len(corpus))
	}

	h.Tmux.WaitForContent(t, pane, scriptedagent.ReadRawDone)

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read captured input: %v", err)
	}
	if string(got) != string(corpus) {
		t.Errorf("captured input differs from what was sent:\n got  %q\n want %q", got, corpus)
	}
}

// TestInput_CtrlCInterruptsForegroundProcess proves Ctrl-C is usable as a real
// interrupt, not just a byte: injected into a pane whose pty is in its normal
// (cooked) mode, 0x03 reaches the foreground process as SIGINT. A shell with a
// SIGINT trap parked on sleep runs its handler when the byte lands.
func TestInput_CtrlCInterruptsForegroundProcess(t *testing.T) {
	t.Parallel()
	h := Start(t)

	// The shell traps SIGINT to print a marker, then loops so the pane
	// survives the interrupt — proving Ctrl-C was delivered as a signal
	// without racing the marker's render against the pane's exit.
	pane := h.Tmux.NewWindow(t, "interruptible",
		`trap 'printf "INTERRUPTED\n"' INT; printf "READY\n"; while true; do sleep 86400; done`)
	waitForPaneInHerd(t, h.Client, pane)

	// Observe through the Client's render stream — the path a real Client
	// uses — rather than tmux capture-pane, which can race a just-created
	// pane while a control client is attached.
	view := watchPane(t, h.Client, pane)
	view.waitFor("READY")

	sendInput(t, h.Client, pane, []byte{0x03}) // Ctrl-C -> SIGINT to the foreground job

	view.waitFor("INTERRUPTED")
}

// TestInput_RoundTripLatency measures and records input round-trip latency on
// the local socket: from sending a keystroke to seeing its echo arrive as
// rendered damage, through inject -> pty echo -> emulator -> damage coalescer
// (16ms) -> wire -> the client-side fold. It asserts a comfortable-for-typing
// ceiling and logs the distribution for the PR record.
func TestInput_RoundTripLatency(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.Tmux.NewWindow(t, "echo", "cat") // echoes typed input back
	waitForPaneInHerd(t, h.Client, pane)

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	in, err := h.Client.SendInput(ctx)
	if err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	defer in.Close() //nolint:errcheck // measurement teardown

	view := watchPane(t, h.Client, pane)
	view.step() // initial resize
	view.step() // initial (blank) sync damage

	const samples = 8
	var best, worst, total time.Duration
	best = time.Hour
	for i := 0; i < samples; i++ {
		ch := byte('a' + i) // a distinct, not-yet-present character each round
		start := time.Now()
		if err := in.SendKeys(pane, []byte{ch}); err != nil {
			t.Fatalf("SendKeys: %v", err)
		}
		for !strings.ContainsRune(view.dump(), rune(ch)) {
			view.step()
		}
		d := time.Since(start)
		t.Logf("sample %d (%q): %s", i, string(rune(ch)), d.Round(time.Microsecond))
		if d < best {
			best = d
		}
		if d > worst {
			worst = d
		}
		total += d
	}

	mean := total / samples
	t.Logf("input round-trip latency over %d samples: min %s, mean %s, max %s",
		samples, best.Round(time.Microsecond), mean.Round(time.Microsecond), worst.Round(time.Microsecond))

	// A generous ceiling: comfortable typing needs tens of milliseconds, and
	// the coalescer caps a single keystroke's added delay at 16ms. This gate
	// catches gross regressions without flaking on a loaded CI box.
	if best > 250*time.Millisecond {
		t.Errorf("best round-trip latency %s exceeds the typing-comfort ceiling of 250ms", best)
	}
}
