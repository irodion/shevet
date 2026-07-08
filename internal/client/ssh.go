package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/irodion/shevet/internal/paths"
	"github.com/irodion/shevet/internal/sshx"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// healthTimeout bounds the eager probe RPC that DialSSH uses to confirm the
// transport reaches a live Server before handing back a Client.
const healthTimeout = 15 * time.Second

// SSHOptions tunes the SSH transport. The zero value is the intended default:
// reach ~/.shevet/shevet.sock on the Host, run `shevet _proxy` as the
// fallback, and log nothing.
type SSHOptions struct {
	// ConfigFile passes an explicit ssh_config to ssh (its -F flag) instead
	// of the user's default ~/.ssh/config. Empty uses the default.
	ConfigFile string

	// RemoteSocket overrides the Server socket path on the Host. Empty means
	// the conventional path resolved against the Host's home directory.
	RemoteSocket string

	// RemoteCommand is the shevet invocation used for the proxy fallback.
	// Empty means {"shevet", "_proxy"} — the binary on the Host's PATH,
	// placed there by the bootstrap slice (issue #5). The resolved socket
	// path is appended so both transports target the same socket.
	RemoteCommand []string

	// Logger receives transport diagnostics (which path engaged, why a
	// fallback happened). Nil discards them.
	Logger *slog.Logger
}

// DialSSH reaches a Server on a remote Host over SSH, honoring the developer's
// ~/.ssh/config, agent, and known_hosts (ADR-0003). It prefers a direct
// channel to the Server's unix socket and falls back to `shevet _proxy` when
// the Host's sshd disables streamlocal forwarding. It returns only after a
// health probe confirms the Server answers, so a returned error is actionable
// (authentication, host key, or no reachable Server) — inspect it with
// errors.Is against sshx.ErrAuth and sshx.ErrHostKey.
func DialSSH(ctx context.Context, host string, opts SSHOptions) (*Client, error) {
	cfg, err := sshx.ResolveConfig(ctx, host, sshx.ResolveOptions{ConfigFile: opts.ConfigFile})
	if err != nil {
		return nil, err
	}
	conn, err := sshx.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}

	transport, err := newSSHTransport(ctx, conn, opts)
	if err != nil {
		conn.Close() //nolint:errcheck // failed setup
		return nil, err
	}

	grpcConn, err := grpc.NewClient(
		"passthrough:///shevet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(transport.dial),
	)
	if err != nil {
		conn.Close() //nolint:errcheck // failed setup
		return nil, fmt.Errorf("create client for %s: %w", host, err)
	}

	c := &Client{
		conn:      grpcConn,
		herd:      shevetv1.NewHerdServiceClient(grpcConn),
		transport: conn,
	}
	if err := c.probe(ctx, transport); err != nil {
		c.Close() //nolint:errcheck // failed setup
		return nil, err
	}
	return c, nil
}

// probe forces the transport to establish and the Server to answer, turning a
// blank-dashboard-then-timeout into a clear error at connect time. When the
// proxy fallback carried the failure, its remote diagnostic (missing socket,
// dead Server) is preferred over gRPC's generic "unavailable".
func (c *Client) probe(ctx context.Context, t *sshTransport) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	if _, err := c.herd.ListPanes(ctx, &shevetv1.ListPanesRequest{}); err != nil {
		if remote := t.lastRemoteErr(); remote != nil {
			return fmt.Errorf("reach server: %w", remote)
		}
		return fmt.Errorf("reach server: %w", err)
	}
	return nil
}

// sshTransport is the gRPC context dialer over one SSH connection. It prefers
// the streamlocal channel and, once that is refused, remembers to use the
// `shevet _proxy` fallback for every subsequent (re)dial.
type sshTransport struct {
	conn       *sshx.Conn
	remotePath string
	proxyArgv  []string
	log        *slog.Logger

	mu              sync.Mutex
	streamLocalDown bool
	lastConn        net.Conn
}

func newSSHTransport(ctx context.Context, conn *sshx.Conn, opts SSHOptions) (*sshTransport, error) {
	remotePath := opts.RemoteSocket
	if remotePath == "" {
		home, err := conn.RemoteHome(ctx)
		if err != nil {
			return nil, err
		}
		remotePath = path.Join(home, paths.SocketRel())
	}

	proxyArgv := opts.RemoteCommand
	if len(proxyArgv) == 0 {
		proxyArgv = []string{"shevet", "_proxy"}
	}
	// Pass the resolved socket explicitly so streamlocal and the proxy target
	// the same path even when it is non-default.
	proxyArgv = append(append([]string{}, proxyArgv...), remotePath)

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &sshTransport{conn: conn, remotePath: remotePath, proxyArgv: proxyArgv, log: log}, nil
}

// dial is the grpc.WithContextDialer callback. gRPC may call it more than once
// (initial connect, reconnects); the streamlocal-down decision is cached so a
// Host without streamlocal doesn't re-probe it every time.
func (t *sshTransport) dial(_ context.Context, _ string) (net.Conn, error) {
	t.mu.Lock()
	down := t.streamLocalDown
	t.mu.Unlock()

	if !down {
		nc, err := t.conn.DialStreamLocal(t.remotePath)
		if err == nil {
			t.log.Debug("ssh transport: streamlocal channel open", "socket", t.remotePath)
			t.remember(nc)
			return nc, nil
		}
		if !errors.Is(err, sshx.ErrStreamLocalUnsupported) {
			return nil, err // a real connection problem, not a forwarding refusal
		}
		t.log.Info("ssh transport: streamlocal unavailable, using proxy fallback", "reason", err)
		t.mu.Lock()
		t.streamLocalDown = true
		t.mu.Unlock()
	}

	nc, err := t.conn.DialCommand(t.proxyArgv)
	if err != nil {
		return nil, err
	}
	t.remember(nc)
	return nc, nil
}

func (t *sshTransport) remember(nc net.Conn) {
	t.mu.Lock()
	t.lastConn = nc
	t.mu.Unlock()
}

// lastRemoteErr returns the diagnostic of the most recent connection when it
// carried a remote process (the proxy fallback) that has since failed.
func (t *sshTransport) lastRemoteErr() error {
	t.mu.Lock()
	nc := t.lastConn
	t.mu.Unlock()
	if ec, ok := nc.(interface{ Err() error }); ok {
		return ec.Err()
	}
	return nil
}
