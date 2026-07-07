// Package harness is the Level-1 e2e harness: a hermetic tmux sandbox, a
// Shevet Server, and the typed gRPC client, composed for tests
// (ARCHITECTURE.md; PRD #1 "Testing Decisions").
//
// Everything is isolated per test: tmux runs its own server on a private
// socket (never the developer's tmux), the Shevet Server listens on a
// private socket, and teardown kills only the sandbox. Agents inside the
// sandbox are deterministic scripted-agent fixtures (`shevet _agent`,
// docs/testing/scripted-agent.md) — the `await-line` step plus SendLine is
// the no-wall-clock-races synchronization contract.
package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/testutil"
)

// waitTimeout bounds every poll against the sandbox. Polling exists only at
// the process boundary (tmux is a separate process); in-process waits are
// synchronous by construction.
const waitTimeout = 10 * time.Second

// Tmux is a hermetic tmux server on a private socket.
type Tmux struct {
	socket string
}

// StartTmux boots an isolated tmux server (private socket, no user config)
// and registers teardown of that server — and only that server — with the
// test. Tests are skipped when tmux is not installed; CI installs it.
func StartTmux(t *testing.T) *Tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; the harness requires it (CI installs it)")
	}

	tm := &Tmux{socket: filepath.Join(testutil.ShortDir(t), "tmux.sock")}

	// A tmux server lives only while it has sessions: keep one holder
	// session running a long sleep. -f /dev/null shuts out user config.
	tm.Run(t, "-f", "/dev/null", "new-session", "-d", "-s", "holder", "-x", "80", "-y", "24", "sleep 86400")
	t.Cleanup(func() {
		out, err := tm.Cmd("kill-server").CombinedOutput()
		if err != nil && !strings.Contains(string(out), "no server") {
			t.Errorf("tmux kill-server: %v\n%s", err, out)
		}
	})

	// Panes must outlive their command so exit codes remain inspectable.
	tm.Run(t, "set-option", "-g", "remain-on-exit", "on")
	return tm
}

// Cmd returns an exec.Cmd for a tmux subcommand against the sandbox socket.
// TMUX is scrubbed from the environment so the sandbox works identically
// whether or not the test itself runs inside a tmux session.
func (tm *Tmux) Cmd(args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", append([]string{"-S", tm.socket}, args...)...)
	cmd.Env = append(environWithout("TMUX"), "TMUX=")
	return cmd
}

// Run executes a tmux subcommand and returns its trimmed stdout, failing the
// test on error.
func (tm *Tmux) Run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := tm.Cmd(args...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// NewWindow starts command in a fresh window and returns the pane's unique
// tmux id (%N), the stable way to target it afterwards.
func (tm *Tmux) NewWindow(t *testing.T, name, command string) string {
	t.Helper()
	return tm.Run(t, "new-window", "-d", "-n", name, "-P", "-F", "#{pane_id}", command)
}

// SendLine types text plus Enter into a pane, literally (-l: no key-name
// interpretation). With a scripted agent parked on await-line, this is the
// deterministic "advance the agent" primitive.
func (tm *Tmux) SendLine(t *testing.T, pane, text string) {
	t.Helper()
	tm.Run(t, "send-keys", "-t", pane, "-l", "--", text)
	tm.Run(t, "send-keys", "-t", pane, "Enter")
}

// CapturePane returns the pane's current visible content.
func (tm *Tmux) CapturePane(t *testing.T, pane string) string {
	t.Helper()
	return tm.Run(t, "capture-pane", "-p", "-t", pane)
}

// WaitForContent polls CapturePane until the content contains substr.
func (tm *Tmux) WaitForContent(t *testing.T, pane, substr string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		content := tm.CapturePane(t, pane)
		if strings.Contains(content, substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never showed %q; content:\n%s", pane, substr, content)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// WaitForExit waits for the pane's command to finish (remain-on-exit keeps
// the pane inspectable) and returns its exit status.
func (tm *Tmux) WaitForExit(t *testing.T, pane string) int {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		out := tm.Run(t, "display-message", "-p", "-t", pane, "#{pane_dead} #{pane_dead_status}")
		var dead, status int
		if _, err := fmt.Sscanf(out, "%d %d", &dead, &status); err == nil && dead == 1 {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never exited (state %q)", pane, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// environWithout returns the process environment minus the named variable.
func environWithout(name string) []string {
	prefix := name + "="
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}
