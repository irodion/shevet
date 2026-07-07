// Package testutil holds small helpers shared by Shevet's test packages.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// SocketPath returns a unix socket path inside a fresh temp directory,
// cleaned up with the test.
//
// Deliberately not t.TempDir(): unix socket paths are capped by sun_path
// (~104 bytes on darwin), and t.TempDir() paths — which embed the full test
// name — can exceed it. os.MkdirTemp keeps the prefix short.
func SocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shevet-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}
