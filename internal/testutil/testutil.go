// Package testutil holds small helpers shared by Shevet's test packages.
package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// WaitTimeout bounds every cross-process wait in the test suite. Polling is
// legitimate only at a process boundary (tmux, spawned binaries); in-process
// waits should be synchronous by construction.
const WaitTimeout = 10 * time.Second

// pollInterval is how often Eventually re-checks its condition.
const pollInterval = 20 * time.Millisecond

// Eventually polls cond until it reports done or WaitTimeout elapses, then
// fails the test naming what was awaited and the last observed state.
func Eventually(t *testing.T, desc string, cond func() (done bool, state string)) {
	t.Helper()
	deadline := time.Now().Add(WaitTimeout)
	for {
		done, state := cond()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last state:\n%s", desc, state)
		}
		time.Sleep(pollInterval)
	}
}

// ShortDir returns a fresh temp directory with a short absolute path,
// cleaned up with the test.
//
// Deliberately not t.TempDir(): unix socket paths are capped by sun_path
// (~104 bytes on darwin), and t.TempDir() paths — which embed the full test
// name — can exceed it. os.MkdirTemp keeps the prefix short.
func ShortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shevet-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// SocketPath returns a unix socket path inside a fresh ShortDir.
func SocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(ShortDir(t), "s.sock")
}

// buildOnce compiles the shevet binary at most once per test process.
var buildOnce = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "shevet-bin-*")
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

// BuildBinary returns the path to a freshly built shevet binary, compiling
// it once per test process and sharing it across callers.
func BuildBinary(t *testing.T) string {
	t.Helper()
	bin, err := buildOnce()
	if err != nil {
		t.Fatalf("build shevet binary: %v", err)
	}
	return bin
}
