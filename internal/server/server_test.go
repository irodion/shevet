package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/testutil"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// startServer binds and serves a Server on socketPath. Listen is synchronous,
// so the socket accepts connections as soon as this returns — no polling.
// Cleanup stops the Server and asserts a clean shutdown.
func startServer(t *testing.T, socketPath string) {
	t.Helper()

	srv := New(Options{SocketPath: socketPath}, discardLogger())
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned error on shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of cancellation")
		}
	})
}

// dial connects through the real Client package, so these tests double as
// client<->server integration tests and keep exercising the dial path real
// Clients use.
func dial(t *testing.T, socketPath string) *client.Client {
	t.Helper()
	c, err := client.Dial(socketPath)
	if err != nil {
		t.Fatalf("client.Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestServe_ServesEmptyHerd(t *testing.T) {
	socketPath := testutil.SocketPath(t)
	startServer(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	panes, err := dial(t, socketPath).ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 0 {
		t.Errorf("ListPanes returned %d panes, want 0", len(panes))
	}
}

func TestServe_ShutsDownCleanlyAndRemovesSocket(t *testing.T) {
	socketPath := testutil.SocketPath(t)

	srv := New(Options{SocketPath: socketPath}, discardLogger())
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
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of cancellation")
	}

	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after shutdown (stat err: %v)", err)
	}
}

func TestListen_RefusesSecondServerOnSameSocket(t *testing.T) {
	socketPath := testutil.SocketPath(t)
	startServer(t, socketPath)

	second := New(Options{SocketPath: socketPath}, discardLogger())
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

	startServer(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dial(t, socketPath).ListPanes(ctx); err != nil {
		t.Fatalf("ListPanes after stale-socket recovery: %v", err)
	}
}

func TestListen_RestrictsSocketPermissions(t *testing.T) {
	socketPath := testutil.SocketPath(t)
	startServer(t, socketPath)

	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions are %o, want 600", perm)
	}
}

func TestRun_RejectsEmptySocketPath(t *testing.T) {
	srv := New(Options{}, discardLogger())
	if err := srv.Run(context.Background()); err == nil {
		t.Fatal("Run with empty socket path succeeded, want error")
	}
}

func TestServe_RequiresListen(t *testing.T) {
	srv := New(Options{SocketPath: testutil.SocketPath(t)}, discardLogger())
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve before Listen succeeded, want error")
	}
}
