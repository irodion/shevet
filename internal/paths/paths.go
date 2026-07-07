// Package paths owns Shevet's on-Host filesystem conventions: the ~/.shevet
// directory and the names inside it.
//
// Both roles need these. The Server resolves them against its local home;
// the Client's SSH transport (issue #10) resolves the same relative
// convention against a remote Host's home, where the local home directory is
// meaningless. Later slices add the Signal spool and the bootstrap artifact
// cache under the same directory.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// dirName is the per-user Shevet state directory, relative to a home.
	dirName = ".shevet"

	// socketName is the Server's unix socket file inside dirName.
	socketName = "shevet.sock"
)

// SocketRel returns the Server socket path relative to a Host's home
// directory (".shevet/shevet.sock"). Use this when addressing a remote Host.
func SocketRel() string {
	return filepath.Join(dirName, socketName)
}

// DefaultSocket returns the conventional Server socket path resolved against
// the local home directory (~/.shevet/shevet.sock, ARCHITECTURE.md §2).
func DefaultSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, SocketRel()), nil
}
