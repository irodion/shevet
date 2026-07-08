package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/irodion/shevet/internal/testutil"
	"github.com/irodion/shevet/internal/tmuxtest"
)

// TestLivePane_VisibleThroughConnect is the demo of issue #8, as a test: an
// agent running in a sandbox tmux is watched live through the real binary —
// `shevet serve` attached to tmux on one side, `shevet connect` rendering
// on a PTY on the other. Content that appears in the pane after the
// dashboard attached must show up on the developer's screen.
func TestLivePane_VisibleThroughConnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY e2e test is unix-only")
	}
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	tm := tmuxtest.Start(t)

	// A scripted agent in the sandbox: prints, then parks on await-line so
	// the second phase is driven — deterministically — by this test.
	script := filepath.Join(testutil.ShortDir(t), "agent.script")
	if err := os.WriteFile(script, []byte(strings.Join([]string{
		"print live from the sandbox",
		"prompt more > ",
		"await-line",
		"print second phase arrived",
		"prompt done > ",
		"await-line",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write agent script: %v", err)
	}
	pane := tm.NewWindow(t, "agent",
		tmuxtest.ShellQuote(bin)+" _agent --script "+tmuxtest.ShellQuote(script))
	tm.WaitForContent(t, pane, "more >")

	// Retire the sandbox's holder window so the agent is the Herd's first
	// Pane — the one this slice's dashboard watches.
	tm.Run(t, "kill-window", "-t", "holder:0")

	serve := startServe(t, bin, socket, "--tmux-socket", tm.Socket(), "--tmux-session", "holder")
	connect, ptmx, snapshot := startConnectPTY(t, bin, socket)

	// Phase 1: content that predates the attachment (the seed path).
	testutil.Eventually(t, "dashboard to show the agent's output", func() (bool, string) {
		s := snapshot()
		return strings.Contains(s, "live from the sandbox"), fmt.Sprintf("%q", s)
	})

	// Phase 2: advance the agent; the new output must arrive live through
	// control mode -> emulator -> damage -> wire -> Bubble Tea -> PTY.
	tm.SendLine(t, pane, "go")
	testutil.Eventually(t, "dashboard to show the second phase", func() (bool, string) {
		s := snapshot()
		return strings.Contains(s, "second phase arrived"), fmt.Sprintf("%q", s)
	})

	if _, err := ptmx.WriteString("q"); err != nil {
		t.Fatalf("send quit key: %v", err)
	}
	if err := waitFor(connect, testutil.WaitTimeout); err != nil {
		t.Fatalf("connect did not exit cleanly after quit: %v", err)
	}

	if err := serve.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM serve: %v", err)
	}
	if err := waitFor(serve, testutil.WaitTimeout); err != nil {
		t.Fatalf("serve did not exit cleanly: %v", err)
	}
}
