package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/tmuxtest"
)

// TestHarness_ServeAndEmptyHerd is the acceptance test of issue #7: real
// tmux sandbox + running Server + gRPC client, asserting the empty Herd.
func TestHarness_ServeAndEmptyHerd(t *testing.T) {
	t.Parallel()
	h := Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	panes, err := h.Client.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 0 {
		t.Errorf("ListPanes returned %d panes, want 0", len(panes))
	}
}

func TestTmux_SandboxIsHermetic(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)

	sessions := tm.Run(t, "list-sessions", "-F", "#{session_name}")
	if sessions != "holder" {
		t.Errorf("sandbox sessions = %q, want only %q", sessions, "holder")
	}

	// The sandbox socket is private and test-scoped; two sandboxes must
	// not see each other.
	other := tmuxtest.Start(t)
	other.Run(t, "new-window", "-d", "-n", "intruder", "sleep 86400")

	if got := tm.Run(t, "list-windows", "-a", "-F", "#{window_name}"); strings.Contains(got, "intruder") {
		t.Errorf("first sandbox sees the second sandbox's window: %q", got)
	}
}

// TestScriptedAgent_DeterministicLifecycle drives a scripted agent through
// output -> await-line -> output -> exit inside a real sandbox pane: the
// full determinism contract (print, park, advance via SendLine, exit code).
func TestScriptedAgent_DeterministicLifecycle(t *testing.T) {
	t.Parallel()
	h := Start(t)

	pane := h.StartAgent(t, "agent-a", `
print working on task
prompt Proceed? [y/n]
await-line
print resuming
exit 4
`)

	// The agent prints, then parks on await-line: by the time the prompt is
	// visible, everything before it must be too — and nothing after it.
	content := h.Tmux.WaitForContent(t, pane, "Proceed? [y/n]")
	if !strings.Contains(content, "working on task") {
		t.Fatalf("output before the prompt is missing:\n%s", content)
	}
	if strings.Contains(content, "resuming") || strings.Contains(content, exitMarker) {
		t.Fatalf("agent advanced past await-line without input:\n%s", content)
	}

	// Advance it and assert the scripted continuation and exit status.
	h.Tmux.SendLine(t, pane, "y")
	h.Tmux.WaitForContent(t, pane, "resuming")
	if code := h.WaitForAgentExit(t, pane); code != 4 {
		t.Errorf("agent exit status = %d, want 4", code)
	}
}

func TestScriptedAgent_TwoAgentsIndependent(t *testing.T) {
	t.Parallel()
	h := Start(t)

	a := h.StartAgent(t, "agent-a", "prompt A waiting\nawait-line\nexit 0\n")
	b := h.StartAgent(t, "agent-b", "prompt B waiting\nawait-line\nexit 0\n")

	h.Tmux.WaitForContent(t, a, "A waiting")
	h.Tmux.WaitForContent(t, b, "B waiting")

	// Advancing A must not advance B.
	h.Tmux.SendLine(t, a, "go")
	if code := h.WaitForAgentExit(t, a); code != 0 {
		t.Errorf("agent A exit status = %d, want 0", code)
	}
	if content := h.Tmux.CapturePane(t, b); strings.Contains(content, exitMarker) {
		t.Errorf("agent B exited when only A was advanced:\n%s", content)
	}

	h.Tmux.SendLine(t, b, "go")
	if code := h.WaitForAgentExit(t, b); code != 0 {
		t.Errorf("agent B exit status = %d, want 0", code)
	}
}
