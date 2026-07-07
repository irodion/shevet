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

	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// Options configures a Server.
type Options struct {
	// SocketPath is the unix socket the gRPC server listens on.
	// Required; see paths.DefaultSocket for the conventional location.
	SocketPath string

	// Tmux, when non-nil, attaches the Server to a tmux session whose
	// panes become the Herd. Nil serves an empty Herd (useful for tests
	// and for hosts where the session is created later).
	Tmux *TmuxOptions
}

// Server serves the shevet.v1 API for one Host.
// Construct with New; then either Run, or Listen followed by Serve.
type Server struct {
	opts     Options
	registry *Registry
	hub      *paneHub
	log      *slog.Logger
	lis      net.Listener
}

// New returns a Server ready to Run. log must not be nil.
func New(opts Options, log *slog.Logger) *Server {
	return &Server{
		opts:     opts,
		registry: NewRegistry(),
		hub:      newPaneHub(),
		log:      log,
	}
}

// Listen binds the Server's unix socket without serving yet. It fails fast
// on a bad path or a live Server on the same socket. Once it returns, the
// socket accepts connections (they are queued until Serve runs) — callers
// that need readiness, like tests, get it here instead of polling.
func (s *Server) Listen() error {
	if s.lis != nil {
		return errors.New("server is already listening")
	}
	lis, err := listenUnix(s.opts.SocketPath)
	if err != nil {
		return err
	}
	s.lis = lis
	return nil
}

// Serve serves gRPC on the socket bound by Listen until ctx is canceled,
// then shuts down gracefully (in-flight RPCs complete). It returns nil after
// a clean, ctx-initiated shutdown, and an error otherwise.
func (s *Server) Serve(ctx context.Context) error {
	if s.lis == nil {
		return errors.New("Serve called before Listen")
	}
	// The listener removes the socket file on Close; this is a belt-and-
	// braces cleanup for the paths where Close is not reached.
	defer os.Remove(s.opts.SocketPath) //nolint:errcheck // best-effort cleanup

	// The watcher's lifetime nests inside Serve: canceled with ctx, and
	// its pipelines are closed before Serve returns (watcher.run's defer),
	// which in turn releases any WatchPane handlers GracefulStop waits on.
	watchCtx, stopWatcher := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	close(watcherDone)
	defer func() {
		stopWatcher()
		<-watcherDone // watcher teardown (pipelines, tmux client) completes before Serve returns
	}()
	if s.opts.Tmux != nil {
		w, err := attachWatcher(watchCtx, *s.opts.Tmux, s.registry, s.hub, s.log)
		if err != nil {
			return fmt.Errorf("attach to tmux: %w", err)
		}
		watcherDone = make(chan struct{})
		go func() {
			defer close(watcherDone)
			if err := w.run(watchCtx); err != nil {
				// The Server outlives its tmux attachment: it keeps
				// serving an empty Herd so Clients see the outage rather
				// than a vanished endpoint. Reattach is the resilience
				// slice's business.
				s.log.Error("tmux watcher stopped", "error", err)
			}
		}()
		s.log.Info("watching tmux session", "session", s.opts.Tmux.Session, "tmux_socket", s.opts.Tmux.Socket)
	}

	grpcServer := grpc.NewServer()
	shevetv1.RegisterHerdServiceServer(grpcServer, newHerdService(s.registry, s.hub))

	s.log.Info("server listening", "socket", s.opts.SocketPath)

	unregister := context.AfterFunc(ctx, func() {
		s.log.Info("shutting down", "reason", context.Cause(ctx))
		grpcServer.GracefulStop()
	})
	defer unregister()

	// Serve returns nil when GracefulStop initiated the stop, and
	// ErrServerStopped when ctx was canceled before serving began.
	if err := grpcServer.Serve(s.lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("grpc serve: %w", err)
	}
	return nil
}

// Run is Listen followed by Serve: the whole Server lifecycle in one call.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
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
// alive and this one must not start. Anything that is not a unix socket is
// left untouched: a mistyped --socket must never delete a user's file.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect existing socket %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s already exists and is not a unix socket; refusing to replace it", path)
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
