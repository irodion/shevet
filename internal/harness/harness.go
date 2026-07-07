// Package harness is the Level-1 e2e harness: a hermetic tmux sandbox
// (internal/tmuxtest), a Shevet Server, and the typed gRPC client, composed
// for tests (ARCHITECTURE.md; PRD #1 "Testing Decisions").
//
// Everything is isolated per test: tmux runs on a private socket, the
// Server listens on a private socket, and teardown removes only what the
// test created. Agents inside the sandbox are deterministic scripted-agent
// fixtures (`shevet _agent`, docs/testing/scripted-agent.md) — the
// `await-line` step plus SendLine is the no-wall-clock-races
// synchronization contract.
package harness

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/server"
	"github.com/irodion/shevet/internal/testutil"
	"github.com/irodion/shevet/internal/tmuxtest"
)

// Harness is the composed Level-1 fixture: a hermetic tmux sandbox, a
// running Shevet Server, and the typed gRPC client — the three parties of
// every e2e scenario. Construct with Start.
type Harness struct {
	// Tmux is the sandbox that hosts Agents.
	Tmux *tmuxtest.Tmux

	// Client is the headless gRPC client attached to the Server.
	Client *client.Client

	// dir holds the Server socket and agent scripts.
	dir string
}

// Start boots the full harness and registers teardown with the test.
func Start(t *testing.T) *Harness {
	t.Helper()

	tm := tmuxtest.Start(t)
	dir := testutil.ShortDir(t)
	return &Harness{
		Tmux:   tm,
		Client: StartServer(t, filepath.Join(dir, "s.sock")),
		dir:    dir,
	}
}

// StartServer boots an in-process Server on socketPath and returns a
// connected client; teardown (client close, graceful Server shutdown with a
// clean-exit assertion) registers with the test.
//
// In-process is deliberate: Listen is synchronous — readiness by
// construction, no polling — and the race detector sees the Server. The
// process boundary itself is covered by the e2e smoke tests.
func StartServer(t *testing.T, socketPath string) *client.Client {
	t.Helper()

	srv := server.New(server.Options{SocketPath: socketPath}, slog.New(slog.DiscardHandler))
	if err := srv.Listen(); err != nil {
		t.Fatalf("server Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server Serve returned error on shutdown: %v", err)
			}
		case <-time.After(testutil.WaitTimeout):
			t.Error("server did not shut down within the deadline")
		}
	})

	c, err := client.Dial(socketPath)
	if err != nil {
		t.Fatalf("client Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// exitMarker is printed into the pane after a StartAgent process ends. It is
// how WaitForAgentExit learns the exit code: tmux's #{pane_dead_status} is
// not portable (3.4 leaves it empty for status 0; 3.7 reports it), so the
// harness relies on pane content instead of pane formats.
const exitMarker = "__agent-exit__:"

// StartAgent writes script to a file and launches `shevet _agent` with it in
// a fresh sandbox window, returning the pane id. The binary is the real
// shevet artifact (built once per test process).
//
// After the agent terminates, the window command prints the exitMarker and
// then parks, holding the pane open so the marker stays inspectable with
// stock tmux options — no remain-on-exit, and no race against a fast agent.
// The parked pane dies with the sandbox.
func (h *Harness) StartAgent(t *testing.T, name, script string) string {
	t.Helper()

	path := filepath.Join(h.dir, name+".script")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write agent script: %v", err)
	}

	command := fmt.Sprintf("%s _agent --script %s; printf '%s%%d\\n' \"$?\"; sleep 86400",
		tmuxtest.ShellQuote(testutil.BuildBinary(t)), tmuxtest.ShellQuote(path), exitMarker)
	return h.Tmux.NewWindow(t, name, command)
}

// WaitForAgentExit waits for a StartAgent pane's process to finish and
// returns its exit code (from the exitMarker line).
func (h *Harness) WaitForAgentExit(t *testing.T, pane string) int {
	t.Helper()
	content := h.Tmux.WaitForContent(t, pane, exitMarker)

	tail := content[strings.LastIndex(content, exitMarker)+len(exitMarker):]
	var code int
	if _, err := fmt.Sscanf(tail, "%d", &code); err != nil {
		t.Fatalf("unparsable exit marker in pane %s: %q", pane, tail)
	}
	return code
}
