package cli

import (
	"context"
	"fmt"
	"io"
)

// runProxy is the hidden stdio<->socket pump used as the SSH tunnel fallback
// when sshd lacks streamlocal forwarding (docs/adr/0003-grpc-over-ssh-tunnel.md).
// It ships with the SSH transport slice (issue #10); until then it fails
// loudly rather than pretending to pump.
func runProxy(ctx context.Context, args []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "shevet _proxy: not implemented yet (arrives with the SSH transport slice)")
	return exitError
}
