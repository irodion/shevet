package server_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/harness"
	"github.com/irodion/shevet/internal/server"
	"github.com/irodion/shevet/internal/testutil"
	"github.com/irodion/shevet/internal/tmuxtest"
)

// The boot-and-connect choreography lives in harness.StartServer, shared
// with the Level-1 harness — these tests double as client<->server
// integration tests and keep exercising the dial path real Clients use.

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestServe_SeedsPreexistingPaneContent covers attaching to a tmux session
// whose panes already have content: the Server must reconstruct the visible
// screen (the capture-pane seed) rather than starting from a blank grid.
func TestServe_SeedsPreexistingPaneContent(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)

	pane := tm.NewWindow(t, "existing", "printf '\\033[32mgreen\\033[0m seeded'; sleep 86400")
	tm.WaitForContent(t, pane, "seeded")

	c := harness.StartServer(t, server.Options{
		SocketPath: testutil.SocketPath(t),
		Tmux:       &server.TmuxOptions{Socket: tm.Socket(), Session: "holder"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
	defer cancel()
	watch, err := c.WatchPane(ctx, pane)
	if err != nil {
		t.Fatalf("WatchPane: %v", err)
	}

	g := grid.New(0, 0)
	for g.RowText(0) != "green seeded" {
		u, err := watch.Recv()
		if err != nil {
			t.Fatalf("Recv: %v (row so far %q)", err, g.RowText(0))
		}
		if u.Resized != nil {
			g = grid.New(u.Resized[0], u.Resized[1])
		}
		for _, p := range u.Damage {
			g.Apply(p)
		}
	}

	// Styling survives the seed: capture-pane -e carries the SGR codes.
	if cell := g.At(0, 0); cell.FG == 0 {
		t.Errorf("seeded cell = %+v, want a green foreground", cell)
	}
}

func TestServe_FailsFastOnMissingTmuxSession(t *testing.T) {
	t.Parallel()
	tm := tmuxtest.Start(t)

	srv := server.New(server.Options{
		SocketPath: testutil.SocketPath(t),
		Tmux:       &server.TmuxOptions{Socket: tm.Socket(), Session: "no-such-session"},
	}, discardLogger())
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve with a missing tmux session succeeded, want error")
	}
}

func TestServe_ServesEmptyHerd(t *testing.T) {
	c := harness.StartServer(t, server.Options{SocketPath: testutil.SocketPath(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 0 {
		t.Errorf("ListPanes returned %d panes, want 0", len(panes))
	}
}

func TestServe_ShutsDownCleanlyAndRemovesSocket(t *testing.T) {
	socketPath := testutil.SocketPath(t)

	srv := server.New(server.Options{SocketPath: socketPath}, discardLogger())
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after cancellation, want nil", err)
		}
	case <-time.After(testutil.WaitTimeout):
		t.Fatal("Serve did not return after cancellation")
	}

	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after shutdown (stat err: %v)", err)
	}
}

func TestListen_RefusesSecondServerOnSameSocket(t *testing.T) {
	socketPath := testutil.SocketPath(t)
	harness.StartServer(t, server.Options{SocketPath: socketPath})

	second := server.New(server.Options{SocketPath: socketPath}, discardLogger())
	if err := second.Listen(); err == nil {
		t.Fatal("second Listen on the same socket succeeded, want error")
	}
}

func TestListen_ClearsStaleSocket(t *testing.T) {
	socketPath := testutil.SocketPath(t)

	// Fabricate an unclean shutdown: a socket file with no listener behind it.
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	lis.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := lis.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("stale socket file was not left behind: %v", err)
	}

	c := harness.StartServer(t, server.Options{SocketPath: socketPath})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ListPanes(ctx); err != nil {
		t.Fatalf("ListPanes after stale-socket recovery: %v", err)
	}
}

func TestListen_RefusesToReplaceNonSocketFile(t *testing.T) {
	socketPath := testutil.SocketPath(t)

	// A user typo: --socket pointing at an existing regular file.
	const content = "precious data"
	if err := os.WriteFile(socketPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	srv := server.New(server.Options{SocketPath: socketPath}, discardLogger())
	if err := srv.Listen(); err == nil {
		t.Fatal("Listen over a regular file succeeded, want error")
	}

	got, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatalf("the file was removed: %v", err)
	}
	if string(got) != content {
		t.Errorf("file content changed: got %q, want %q", got, content)
	}
}

func TestListen_RestrictsSocketPermissions(t *testing.T) {
	socketPath := testutil.SocketPath(t)
	harness.StartServer(t, server.Options{SocketPath: socketPath})

	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions are %o, want 600", perm)
	}
}

func TestRun_RejectsEmptySocketPath(t *testing.T) {
	srv := server.New(server.Options{}, discardLogger())
	if err := srv.Run(context.Background()); err == nil {
		t.Fatal("Run with empty socket path succeeded, want error")
	}
}

func TestServe_RequiresListen(t *testing.T) {
	srv := server.New(server.Options{SocketPath: testutil.SocketPath(t)}, discardLogger())
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve before Listen succeeded, want error")
	}
}
