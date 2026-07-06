// Package server implements the Shevet Server: the daemon started by
// `shevet serve` on a Host.
//
// The Server exposes the shevet.v1 gRPC API on a unix socket. There is no
// TCP listener by design: Clients reach the socket through an SSH tunnel,
// and SSH is the sole authentication and authorization boundary
// (docs/adr/0003-grpc-over-ssh-tunnel.md).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"

	"github.com/irodion/shevet/internal/herd"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// DefaultSocketPath returns the conventional Server socket location,
// ~/.shevet/shevet.sock (see ARCHITECTURE.md §2).
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".shevet", "shevet.sock"), nil
}

// Options configures a Server.
type Options struct {
	// SocketPath is the unix socket the gRPC server listens on.
	// Required; see DefaultSocketPath for the conventional location.
	SocketPath string
}

// Server serves the shevet.v1 API for one Host.
// Construct with New; run with Run.
type Server struct {
	opts     Options
	registry *herd.Registry
	log      *slog.Logger
}

// New returns a Server ready to Run. log must not be nil.
func New(opts Options, log *slog.Logger) *Server {
	return &Server{
		opts:     opts,
		registry: herd.NewRegistry(),
		log:      log,
	}
}

// Run listens on the configured unix socket and serves gRPC until ctx is
// canceled, then shuts down gracefully (in-flight RPCs complete). It returns
// nil after a clean, ctx-initiated shutdown, and an error otherwise.
func (s *Server) Run(ctx context.Context) error {
	lis, err := listenUnix(s.opts.SocketPath)
	if err != nil {
		return err
	}
	// The listener removes the socket file on Close; this is a belt-and-
	// braces cleanup for the paths where Close is not reached.
	defer os.Remove(s.opts.SocketPath) //nolint:errcheck // best-effort cleanup

	grpcServer := grpc.NewServer()
	shevetv1.RegisterHerdServiceServer(grpcServer, newHerdService(s.registry))

	s.log.Info("server listening", "socket", s.opts.SocketPath)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- grpcServer.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		s.log.Info("shutting down", "reason", context.Cause(ctx))
		grpcServer.GracefulStop()
		<-serveErr // Serve returns once Stop completes; error is moot here.
		return nil
	case err := <-serveErr:
		return fmt.Errorf("grpc serve: %w", err)
	}
}

// listenUnix prepares and listens on a unix socket path: it creates the
// parent directory (private to the user), detects and refuses a live Server
// on the same socket, clears stale leftovers from an unclean shutdown, and
// restricts the socket itself to the owning user.
func listenUnix(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("socket path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}

	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on unix socket %s: %w", path, err)
	}
	// SSH is the authn boundary, but the socket is still kept private to the
	// owning user as defense in depth on multi-user Hosts.
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close() //nolint:errcheck // already failing; listener cleanup only
		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}
	return lis, nil
}

// clearStaleSocket removes a leftover socket file from an unclean shutdown.
// If something is actually accepting connections on it, another Server is
// alive and this one must not start.
func clearStaleSocket(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect existing socket %s: %w", path, err)
	}

	conn, err := net.Dial("unix", path)
	if err == nil {
		conn.Close() //nolint:errcheck // probe connection only
		return fmt.Errorf("a server is already listening on %s", path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}
