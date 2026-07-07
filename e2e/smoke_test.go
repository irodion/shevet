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
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/irodion/shevet/internal/testutil"
)

// startServe launches `shevet serve` and waits until its socket accepts
// connections. It returns the running command; the caller owns shutdown.
func startServe(t *testing.T, bin, socket string, extraArgs ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(bin, append([]string{"serve", "--socket", socket}, extraArgs...)...)
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

	testutil.Eventually(t, "serve socket "+socket+" to accept connections", func() (bool, string) {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return false, err.Error()
		}
		conn.Close()
		return true, ""
	})
	return cmd
}

func TestSmoke_ServeSIGTERMShutsDownCleanly(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	cmd := startServe(t, bin, socket)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if err := waitFor(cmd, testutil.WaitTimeout); err != nil {
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
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	serve := startServe(t, bin, socket)

	connect, ptmx, snapshot := startConnectPTY(t, bin, socket)

	testutil.Eventually(t, `dashboard to render "0 Panes"`, func() (bool, string) {
		s := snapshot()
		return strings.Contains(s, "0 Panes"), fmt.Sprintf("%q", s)
	})

	if _, err := ptmx.WriteString("q"); err != nil {
		t.Fatalf("send quit key: %v", err)
	}
	if err := waitFor(connect, testutil.WaitTimeout); err != nil {
		t.Fatalf("connect did not exit cleanly after quit: %v", err)
	}

	if err := serve.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM serve: %v", err)
	}
	if err := waitFor(serve, testutil.WaitTimeout); err != nil {
		t.Fatalf("serve did not exit cleanly: %v", err)
	}
}

// startConnectPTY runs `shevet connect` on a real PTY and returns the
// command, the PTY master, and a snapshot function accumulating everything
// the dashboard has drawn.
func startConnectPTY(t *testing.T, bin, socket string) (*exec.Cmd, *os.File, func() string) {
	t.Helper()

	connect := exec.Command(bin, "connect", "--socket", socket)
	connect.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.StartWithSize(connect, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("start connect on PTY: %v", err)
	}
	t.Cleanup(func() {
		ptmx.Close() //nolint:errcheck // PTY teardown
		if connect.ProcessState == nil {
			connect.Process.Kill() //nolint:errcheck // last-resort cleanup
			connect.Wait()         //nolint:errcheck
		}
	})

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
	return connect, ptmx, snapshot
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
