// Package tmuxtest provides a hermetic tmux sandbox for tests: a private
// tmux server on a private socket, torn down with the test, never touching
// a developer's own tmux.
//
// It is deliberately a leaf (no shevet imports beyond testutil) so any
// layer can use it — the Level-1 harness composes it around a Server, and
// the control-mode slice's white-box Server tests can import it directly
// without an import cycle.
//
// The sandbox keeps stock tmux behavior (no option overrides): the Server
// under test must see the same pane-lifecycle semantics a user's tmux has.
// Tests that need divergent options (e.g. remain-on-exit for exit-status
// assertions) set them per window.
package tmuxtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/irodion/shevet/internal/testutil"
)

// Tmux is a hermetic tmux server on a private socket.
type Tmux struct {
	socket string
	env    []string
}

// Start boots an isolated tmux server (private socket, no user config) and
// registers teardown of that server — and only that server — with the test.
// Tests are skipped when tmux is not installed; CI installs it.
func Start(t *testing.T) *Tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; the sandbox requires it (CI installs it)")
	}

	tm := &Tmux{
		socket: filepath.Join(testutil.ShortDir(t), "tmux.sock"),
		// TMUX is scrubbed once so the sandbox works identically whether
		// or not the test itself runs inside a tmux session.
		env: slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "TMUX=")
		}),
	}

	// A tmux server lives only while it has sessions: keep one holder
	// session running a long sleep. -f /dev/null shuts out user config.
	tm.Run(t, "-f", "/dev/null", "new-session", "-d", "-s", "holder", "-x", "80", "-y", "24", "sleep 86400")
	t.Cleanup(func() {
		out, err := tm.Cmd("kill-server").CombinedOutput()
		if err != nil && !strings.Contains(string(out), "no server") {
			t.Errorf("tmux kill-server: %v\n%s", err, out)
		}
	})
	return tm
}

// Socket returns the sandbox tmux server's socket path, for components that
// dial tmux themselves (e.g. a control-mode attachment).
func (tm *Tmux) Socket() string {
	return tm.socket
}

// Cmd returns an exec.Cmd for a tmux subcommand against the sandbox socket.
func (tm *Tmux) Cmd(args ...string) *exec.Cmd {
	cmd := exec.Command("tmux", append([]string{"-S", tm.socket}, args...)...)
	cmd.Env = tm.env
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

// NewWindow starts a shell command in a fresh window and returns the pane's
// unique tmux id (%N), the stable way to target it afterwards. The command
// is interpreted by /bin/sh; quote embedded paths with ShellQuote.
func (tm *Tmux) NewWindow(t *testing.T, name, command string) string {
	t.Helper()
	return tm.Run(t, "new-window", "-d", "-n", name, "-P", "-F", "#{pane_id}", command)
}

// SendLine types text plus a carriage return into a pane, literally (-l: no
// key-name interpretation). With a scripted agent parked on await-line, this
// is the deterministic "advance the agent" primitive.
func (tm *Tmux) SendLine(t *testing.T, pane, text string) {
	t.Helper()
	tm.Run(t, "send-keys", "-t", pane, "-l", "--", text+"\r")
}

// CapturePane returns the pane's current visible content.
func (tm *Tmux) CapturePane(t *testing.T, pane string) string {
	t.Helper()
	return tm.Run(t, "capture-pane", "-p", "-t", pane)
}

// WaitForContent polls CapturePane until the content contains substr, and
// returns the content that satisfied the wait.
func (tm *Tmux) WaitForContent(t *testing.T, pane, substr string) string {
	t.Helper()
	var content string
	testutil.Eventually(t, "pane "+pane+" to show "+substr, func() (bool, string) {
		content = tm.CapturePane(t, pane)
		return strings.Contains(content, substr), content
	})
	return content
}

// ShellQuote returns s single-quoted for /bin/sh, safe for embedding in a
// NewWindow command (paths with spaces or metacharacters included).
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
