package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// testSocketPath returns a unix socket path short enough for the platform
// limit (~104 bytes on darwin), cleaned up with the test.
func testSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shevet-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "shevet.sock")
}

// startServer runs a Server on socketPath and returns once it accepts
// connections. Cleanup stops it and asserts a clean shutdown.
func startServer(t *testing.T, socketPath string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Options{SocketPath: socketPath}, discardLogger())

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned error on shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return within 5s of cancellation")
		}
	})

	waitForSocket(t, socketPath, done)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// waitForSocket polls until the socket accepts a connection, failing fast if
// the server exits first.
func waitForSocket(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("server exited before accepting connections: %v", err)
		default:
		}
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server socket %s never became dialable", path)
}

func dialTestClient(t *testing.T, socketPath string) shevetv1.HerdServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(
		"unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return shevetv1.NewHerdServiceClient(conn)
}

func TestRun_ServesEmptyHerd(t *testing.T) {
	socketPath := testSocketPath(t)
	startServer(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := dialTestClient(t, socketPath).ListPanes(ctx, &shevetv1.ListPanesRequest{})
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if n := len(resp.GetPanes()); n != 0 {
		t.Errorf("ListPanes returned %d panes, want 0", n)
	}
}

func TestRun_ShutsDownCleanlyAndRemovesSocket(t *testing.T) {
	socketPath := testSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Options{SocketPath: socketPath}, discardLogger())

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitForSocket(t, socketPath, done)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}

	if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after shutdown (stat err: %v)", err)
	}
}

func TestRun_RefusesSecondServerOnSameSocket(t *testing.T) {
	socketPath := testSocketPath(t)
	startServer(t, socketPath)

	second := New(Options{SocketPath: socketPath}, discardLogger())
	err := second.Run(context.Background())
	if err == nil {
		t.Fatal("second Run on the same socket succeeded, want error")
	}
}

func TestRun_ClearsStaleSocket(t *testing.T) {
	socketPath := testSocketPath(t)

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
	if _, err := dialTestClient(t, socketPath).ListPanes(ctx, &shevetv1.ListPanesRequest{}); err != nil {
		t.Fatalf("ListPanes after stale-socket recovery: %v", err)
	}
}

func TestRun_RestrictsSocketPermissions(t *testing.T) {
	socketPath := testSocketPath(t)
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
