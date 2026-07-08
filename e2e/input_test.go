package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/scriptedagent"
	"github.com/irodion/shevet/internal/testutil"
	"github.com/irodion/shevet/internal/tmuxtest"
)

// TestInput_TypeThroughConnect is the demo of issue #9 as a test: a developer
// types into a Pane through the real Client. It drives `shevet connect` on a
// PTY — entering passthrough, typing a line — and a scripted agent capturing
// its stdin in raw mode receives exactly what was typed. This exercises the
// whole Client input path end to end: terminal bytes -> Bubble Tea key
// parsing -> the Client's key encoder -> Control Input stream -> injector ->
// tmux -> the Pane's pty.
func TestInput_TypeThroughConnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY e2e test is unix-only")
	}
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	tm := tmuxtest.Start(t)

	// The typed line: ASCII, an accented UTF-8 rune (≥0x80, the literal
	// path), and Enter. Byte-diverse control/CJK/emoji coverage lives in the
	// harness byte-identity test; here the point is the live Client path.
	payload := []byte("hi café\r")
	out := filepath.Join(testutil.ShortDir(t), "typed.bin")

	// Park on await-line after the capture so the agent — the Herd's only
	// Pane — keeps the tmux session alive, letting the Done marker render
	// instead of racing the pane's exit.
	script := filepath.Join(testutil.ShortDir(t), "agent.script")
	scriptBody := fmt.Sprintf("read-raw %d %s\nawait-line\n", len(payload), out)
	if err := os.WriteFile(script, []byte(scriptBody), 0o600); err != nil {
		t.Fatalf("write agent script: %v", err)
	}
	pane := tm.NewWindow(t, "agent",
		tmuxtest.ShellQuote(bin)+" _agent --script "+tmuxtest.ShellQuote(script))

	// Wait for raw mode before anything watches the pane (no control client
	// attached yet, so this capture cannot race the pane's creation).
	tm.WaitForContent(t, pane, scriptedagent.ReadRawReady)

	// Retire the sandbox's holder window so the agent is the Herd's first
	// Pane — the one this slice's dashboard watches.
	tm.Run(t, "kill-window", "-t", "holder:0")

	serve := startServe(t, bin, socket, "--tmux-socket", tm.Socket(), "--tmux-session", "holder")
	connect, ptmx, snapshot := startConnectPTY(t, bin, socket)

	// The dashboard is up once it has rendered the agent's ready marker.
	testutil.Eventually(t, "dashboard to show the agent ready to read", func() (bool, string) {
		s := snapshot()
		return strings.Contains(s, scriptedagent.ReadRawReady), fmt.Sprintf("%q", s)
	})

	// Enter passthrough ('i'), then type. The pause lets Bubble Tea process
	// 'i' as its own key and open the input stream; keystrokes typed before
	// it opens are queued by the Model and flushed, so the pause is only to
	// keep 'i' from batching with the payload into one multi-rune key.
	if _, err := ptmx.WriteString("i"); err != nil {
		t.Fatalf("enter passthrough: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := ptmx.Write(payload); err != nil {
		t.Fatalf("type payload: %v", err)
	}

	// The agent prints Done once it has captured every byte.
	testutil.Eventually(t, "agent to capture the typed input", func() (bool, string) {
		s := snapshot()
		return strings.Contains(s, scriptedagent.ReadRawDone), fmt.Sprintf("%q", s)
	})

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read captured input: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("typed input arrived altered:\n got  %q\n want %q", got, payload)
	}

	// Leave passthrough (Ctrl-\), then quit the dashboard.
	if _, err := ptmx.Write([]byte{0x1c}); err != nil {
		t.Fatalf("leave passthrough: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := ptmx.WriteString("q"); err != nil {
		t.Fatalf("quit key: %v", err)
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
