package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	"github.com/irodion/shevet/internal/paths"
)

// runProxy is the hidden stdio<->socket pump used as the SSH tunnel fallback
// when a Host's sshd lacks streamlocal forwarding (ADR-0003). The Client runs
// it over `ssh exec` and speaks gRPC through its stdio; this process just
// copies bytes between stdin/stdout and the Server's unix socket. It is the
// authoritative source of "no server" / "no socket" errors: streamlocal
// cannot distinguish those, but this can, and its stderr reaches the Client.
func runProxy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shevet _proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() > 1 {
		fmt.Fprintf(stderr, "shevet _proxy: expected at most one <socket> argument, got %d\n", flags.NArg())
		return exitUsage
	}

	socketPath := flags.Arg(0)
	if socketPath == "" {
		var err error
		if socketPath, err = paths.DefaultSocket(); err != nil {
			fmt.Fprintf(stderr, "shevet _proxy: %v\n", err)
			return exitError
		}
	}

	if err := proxyPump(ctx, os.Stdin, stdout, socketPath); err != nil {
		fmt.Fprintf(stderr, "shevet _proxy: %v\n", err)
		return exitError
	}
	return exitOK
}

// proxyPump connects to the Server socket and copies bytes both ways between
// it and in/out until either side closes or ctx is canceled. A dial failure
// is returned (the actionable case the Client relays); a normal half-close is
// not an error.
func proxyPump(ctx context.Context, in io.Reader, out io.Writer, socketPath string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil // shutdown signaled before we connected — a clean exit
		}
		return fmt.Errorf("connect to server socket %s: %w", socketPath, describeDialError(err))
	}
	defer conn.Close() //nolint:errcheck // teardown

	// Client -> Server: on stdin EOF, half-close so the Server sees the end
	// of input while we keep draining its side.
	go func() {
		io.Copy(conn, in) //nolint:errcheck // copy ends on EOF/close; teardown handles the rest
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() //nolint:errcheck // best-effort half-close
		}
	}()

	// Server -> Client: this direction defines the connection's lifetime.
	// The Server closing the socket, or the Client's ssh session ending,
	// finishes the copy.
	done := make(chan struct{})
	go func() {
		io.Copy(out, conn) //nolint:errcheck // teardown handles the error path
		close(done)
	}()

	select {
	case <-ctx.Done():
		return nil // signaled shutdown is a clean exit, not a failure
	case <-done:
		return nil
	}
}

// describeDialError sharpens the two unix-socket dial failures a user can act
// on: a missing socket (no Server has ever run / wrong path) versus a refused
// connection (a stale socket file left by a dead Server).
func describeDialError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w (is the Server running on this Host?)", err)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("%w (stale socket — the Server is not listening)", err)
	default:
		return err
	}
}
