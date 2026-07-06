// Package version exposes the build version of the shevet binary.
//
// The version is stamped at build time via:
//
//	-ldflags "-X github.com/irodion/shevet/internal/version.version=v1.2.3"
//
// (see the Makefile). Unstamped builds report "dev".
package version

import "runtime"

var version = "dev"

// String returns the stamped build version, e.g. "v0.1.0" or "dev".
func String() string {
	return version
}

// GoVersion returns the Go toolchain version the binary was built with.
func GoVersion() string {
	return runtime.Version()
}
