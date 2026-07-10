package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/irodion/shevet/internal/testutil"
)

// TestProxyPump verifies the fallback pump copies bytes both ways between the
// caller's stdio and the Server socket, and half-closes cleanly on stdin EOF.
func TestProxyPump(t *testing.T) {
	socket := testutil.SocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck

	// An echo Server: uppercase nothing, just reflect bytes back.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(conn, conn) //nolint:errcheck // ends when the pump half-closes
		conn.Close()        //nolint:errcheck
	}()

	inR, inW := io.Pipe()
	var out testutil.SyncBuffer
	done := make(chan error, 1)
	go func() { done <- proxyPump(context.Background(), inR, &out, socket) }()

	if _, err := inW.Write([]byte("hello over ssh")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	inW.Close() //nolint:errcheck // signal EOF -> half-close -> echo server closes

	if err := <-done; err != nil {
		t.Fatalf("proxyPump returned error: %v", err)
	}
	wg.Wait()
	if got := out.String(); got != "hello over ssh" {
		t.Errorf("echoed output = %q, want %q", got, "hello over ssh")
	}
}

// TestProxyPumpMissingSocket checks the actionable error for a socket that was
// never created — the "no Server on this Host" case the Client relays.
func TestProxyPumpMissingSocket(t *testing.T) {
	missing := filepath.Join(testutil.ShortDir(t), "absent.sock")
	err := proxyPump(context.Background(), strings.NewReader(""), io.Discard, missing)
	if err == nil {
		t.Fatal("expected an error dialing a missing socket, got nil")
	}
	if !strings.Contains(err.Error(), "Server running") {
		t.Errorf("error %q does not explain the missing Server", err)
	}
}

// TestProxyPumpContextCancel checks a canceled context ends the pump cleanly
// (a signaled shutdown is not a failure).
func TestProxyPumpContextCancel(t *testing.T) {
	socket := testutil.SocketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		if conn, err := ln.Accept(); err == nil {
			defer conn.Close()        //nolint:errcheck
			io.Copy(io.Discard, conn) //nolint:errcheck
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// A reader that blocks forever, standing in for an idle stdin.
	go func() { done <- proxyPump(ctx, blockingReader{}, io.Discard, socket) }()

	cancel()
	if err := <-done; err != nil {
		t.Errorf("canceled pump returned %v, want nil", err)
	}
}

func TestDescribeDialError(t *testing.T) {
	if got := describeDialError(errors.New("plain")); got.Error() != "plain" {
		t.Errorf("unknown error rewritten to %q, want passthrough", got)
	}
}

// blockingReader never returns from Read until the process exits, modeling an
// idle stdin that only cancellation can end.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }
