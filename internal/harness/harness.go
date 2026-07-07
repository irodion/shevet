package harness

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/server"
	"github.com/irodion/shevet/internal/testutil"
)

// Harness is the composed Level-1 fixture: a hermetic tmux sandbox, a
// running Shevet Server, and the typed gRPC client — the three parties of
// every e2e scenario. Construct with Start.
type Harness struct {
	// Tmux is the sandbox that hosts Agents.
	Tmux *Tmux

	// Client is the headless gRPC client attached to the Server.
	Client *client.Client

	// SocketPath is the Server's unix socket.
	SocketPath string
}

// Start boots the full harness and registers teardown with the test. The
// Server runs in-process (the process boundary itself is covered by the e2e
// smoke tests), so its Listen is synchronous — no readiness polling — and
// the race detector sees it.
func Start(t *testing.T) *Harness {
	t.Helper()

	tm := StartTmux(t)
	socketPath := testutil.SocketPath(t)

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
		case <-time.After(waitTimeout):
			t.Error("server did not shut down within the deadline")
		}
	})

	c, err := client.Dial(socketPath)
	if err != nil {
		t.Fatalf("client Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	return &Harness{
		Tmux:       tm,
		Client:     c,
		SocketPath: socketPath,
	}
}

// StartAgent writes script to a file and launches `shevet _agent` with it in
// a fresh sandbox window, returning the pane id. The binary is the real
// shevet artifact (built once per test process).
func (h *Harness) StartAgent(t *testing.T, name, script string) string {
	t.Helper()

	path := filepath.Join(testutil.ShortDir(t), name+".script")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write agent script: %v", err)
	}

	bin := testutil.BuildBinary(t)
	return h.Tmux.NewWindow(t, name, bin+" _agent --script "+path)
}
