// Package e2e smoke-tests the built shevet binary end to end: a real Server
// process on a unix socket and a real Client dashboard on a PTY.
//
// These tests exercise the walking skeleton (issue #6); the full Level-1
// harness with a tmux sandbox and scripted Agents arrives with issue #7.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

const waitTimeout = 10 * time.Second

// buildBinary compiles shevet once per test run into a shared temp dir.
var buildBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "shevet-e2e-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "shevet")

	cmd := exec.Command("go", "build", "-o", bin, "github.com/irodion/shevet")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, out)
	}
	return bin, nil
})

func binaryPath(t *testing.T) string {
	t.Helper()
	bin, err := buildBinary()
	if err != nil {
		t.Fatalf("build shevet binary: %v", err)
	}
	return bin
}

// shortSocketPath returns a socket path under the platform length limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shevet-e2e-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// startServe launches `shevet serve` and waits until its socket accepts
// connections. It returns the running command; the caller owns shutdown.
func startServe(t *testing.T, bin, socket string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(bin, "serve", "--socket", socket)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill() //nolint:errcheck // last-resort cleanup
			cmd.Wait()         //nolint:errcheck
		}
	})

	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serve socket %s never became dialable", socket)
	return nil
}

func TestSmoke_ServeSIGTERMShutsDownCleanly(t *testing.T) {
	bin := binaryPath(t)
	socket := shortSocketPath(t)
	cmd := startServe(t, bin, socket)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if err := waitFor(cmd, waitTimeout); err != nil {
		t.Fatalf("serve did not exit cleanly after SIGTERM: %v", err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after shutdown (stat err: %v)", err)
	}
}

func TestSmoke_ConnectRendersEmptyHerdAndQuits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY smoke test is unix-only")
	}
	bin := binaryPath(t)
	socket := shortSocketPath(t)
	serve := startServe(t, bin, socket)

	connect := exec.Command(bin, "connect", "--socket", socket)
	connect.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(connect, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("start connect on PTY: %v", err)
	}
	defer ptmx.Close() //nolint:errcheck // PTY teardown

	// Accumulate PTY output until the dashboard shows the empty Herd.
	var (
		mu  sync.Mutex
		out strings.Builder
	)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}

	deadline := time.Now().Add(waitTimeout)
	for !strings.Contains(snapshot(), "0 Panes") {
		if time.Now().After(deadline) {
			t.Fatalf("dashboard never rendered \"0 Panes\"; output so far:\n%q", snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := ptmx.WriteString("q"); err != nil {
		t.Fatalf("send quit key: %v", err)
	}
	if err := waitFor(connect, waitTimeout); err != nil {
		t.Fatalf("connect did not exit cleanly after quit: %v", err)
	}

	if err := serve.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM serve: %v", err)
	}
	if err := waitFor(serve, waitTimeout); err != nil {
		t.Fatalf("serve did not exit cleanly: %v", err)
	}
}

// waitFor waits for cmd to exit successfully within the timeout.
func waitFor(cmd *exec.Cmd, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cmd.Process.Kill() //nolint:errcheck // unblock Wait
		return fmt.Errorf("process did not exit within %s", timeout)
	}
}
