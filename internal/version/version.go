// Package version exposes the build version of the shevet binary.
//
// The version is stamped at build time via:
//
//	-ldflags "-X github.com/irodion/shevet/internal/version.version=v1.2.3"
//
// (see the Makefile). Unstamped builds report "dev".
package version

var version = "dev"

// String returns the stamped build version, e.g. "v0.1.0" or "dev".
func String() string {
	return version
}
