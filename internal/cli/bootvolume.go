package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// nonBootVolumeWarning is the advisory shown when shevet is launched from a
// volume other than the boot volume. It is deliberately gentle: it recommends
// against the setup rather than predicting a specific failure.
//
// The reason it matters (for maintainers, not the message): a non-boot volume
// can be unmounted or physically disconnected while a long-lived Server or
// Client is running. If that happens, the kernel can no longer page in the
// binary's memory-mapped code and the process wedges — on macOS it spins
// rather than exiting — which is unrecoverable in-process, so the only real
// defense is to run from the boot volume.
const nonBootVolumeWarning = "running from a non-boot volume is not recommended for a long-running Server or " +
	"Client; if the volume is disconnected, shevet may stop working. Prefer a local install (e.g. ~/bin)."

// executableOnNonBootVolume returns the resolved path of the running executable
// and whether it lives on a volume other than the boot volume. It is
// best-effort: any error locating the executable reports false rather than
// failing a role.
func executableOnNonBootVolume() (path string, nonBoot bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	// Resolve symlinks so a launcher symlink on the boot volume pointing at a
	// binary elsewhere (or vice versa) is classified by where the real file
	// lives.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, isNonBootVolumePath(runtime.GOOS, exe)
}

// isNonBootVolumePath is the pure classifier behind executableOnNonBootVolume,
// split out so it is testable without depending on where the test binary runs.
//
// On macOS the boot volume is "/"; every other mounted volume — external USB
// and Thunderbolt disks, mounted disk images, network shares, and secondary
// internal volumes — appears under /Volumes. So a path under /Volumes is not on
// the boot volume. This is the simple, CGO-free stand-in for a device-level
// "same volume as /?" check (ADR-0002 rules out DiskArbitration); it is
// advisory, not a gate. Only macOS is checked: /Volumes is a macOS convention.
func isNonBootVolumePath(goos, path string) bool {
	if goos != "darwin" {
		return false
	}
	return strings.HasPrefix(path, "/Volumes/")
}
