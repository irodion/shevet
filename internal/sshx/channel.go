package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ErrStreamLocalUnsupported reports that the Host's sshd refused to open a
// direct-streamlocal channel — either it disables streamlocal forwarding or
// the target socket is not there. Callers fall back to DialCommand, which can
// tell those two apart. Wrapped, so the underlying reject reason survives.
var ErrStreamLocalUnsupported = errors.New("streamlocal forwarding unavailable")

// streamLocalDirectMsg is the payload of a direct-streamlocal@openssh.com
// channel open: the remote socket path plus two reserved fields the protocol
// requires to be empty/zero (OpenSSH PROTOCOL §2.4).
type streamLocalDirectMsg struct {
	SocketPath string
	Reserved0  string
	Reserved1  uint32
}

// DialStreamLocal opens a direct channel to a unix socket on the Host and
// returns it as a net.Conn — the fast path, one round trip with no remote
// process. When the Host's sshd refuses the channel it returns an error
// wrapping ErrStreamLocalUnsupported, which callers detect with errors.Is to
// fall back to DialCommand.
func (c *Conn) DialStreamLocal(remotePath string) (net.Conn, error) {
	payload := ssh.Marshal(streamLocalDirectMsg{SocketPath: remotePath})
	ch, reqs, err := c.client.OpenChannel("direct-streamlocal@openssh.com", payload)
	if err != nil {
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) {
			// sshd rejected the channel: forwarding disabled or socket absent.
			return nil, fmt.Errorf("%w: %w", ErrStreamLocalUnsupported, err)
		}
		return nil, fmt.Errorf("sshx: open streamlocal channel to %s: %w", remotePath, err)
	}
	go ssh.DiscardRequests(reqs)
	return &streamConn{Channel: ch, sshConnBase: sshConnBase{addr: c.cfg.addr()}, path: remotePath}, nil
}

// DialCommand runs argv on the Host and returns its stdio as a net.Conn:
// writes go to the command's stdin, reads come from its stdout. This is the
// fallback transport — the client runs `shevet _proxy`, which pumps the same
// bytes to the Server socket. If the command exits, reads surface its stderr
// and exit status instead of a bare EOF, so failures stay actionable.
func (c *Conn) DialCommand(argv []string) (net.Conn, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("sshx: open session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close() //nolint:errcheck // failed setup
		return nil, fmt.Errorf("sshx: pipe stdin: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close() //nolint:errcheck // failed setup
		return nil, fmt.Errorf("sshx: pipe stdout: %w", err)
	}
	conn := &cmdConn{sshConnBase: sshConnBase{addr: c.cfg.addr()}, sess: sess, stdin: stdin, stdout: stdout, cmd: strings.Join(argv, " ")}
	sess.Stderr = &conn.stderr

	if err := sess.Start(shellQuote(argv)); err != nil {
		sess.Close() //nolint:errcheck // failed setup
		return nil, fmt.Errorf("sshx: start %q: %w", conn.cmd, err)
	}
	return conn, nil
}

// RemoteHome returns the login user's home directory on the Host, used to
// resolve the conventional Server socket path (~/.shevet/shevet.sock) for the
// streamlocal path. printf avoids the trailing newline echo would add.
func (c *Conn) RemoteHome(ctx context.Context) (string, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("sshx: open session: %w", err)
	}
	defer sess.Close() //nolint:errcheck // read-only session

	stop := context.AfterFunc(ctx, func() { sess.Close() }) //nolint:errcheck // unblock Output
	defer stop()

	out, err := sess.Output(`printf '%s' "$HOME"`)
	if err != nil {
		return "", fmt.Errorf("sshx: resolve remote home: %w", err)
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", fmt.Errorf("sshx: remote home directory is empty")
	}
	return home, nil
}

// shellQuote renders argv as a single command line safe for the remote login
// shell: each argument is wrapped in single quotes, with embedded single
// quotes escaped the POSIX way ('\”).
func shellQuote(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// sshAddr is a synthetic net.Addr for streams that ride the SSH connection;
// there is no real socket address to report on the client side.
type sshAddr struct{ network, addr string }

func (a sshAddr) Network() string { return a.network }
func (a sshAddr) String() string  { return a.addr }

// sshConnBase supplies the net.Conn addressing and deadline methods shared by
// every stream on the SSH connection. The streams have no deadline concept —
// the setters are no-ops and gRPC's own keepalive and per-RPC timeouts govern
// liveness — and their local address is synthetic. RemoteAddr differs per
// stream, so each embedding type provides its own.
type sshConnBase struct{ addr string }

func (b sshConnBase) LocalAddr() net.Addr            { return sshAddr{"ssh", b.addr} }
func (sshConnBase) SetDeadline(time.Time) error      { return nil }
func (sshConnBase) SetReadDeadline(time.Time) error  { return nil }
func (sshConnBase) SetWriteDeadline(time.Time) error { return nil }

// streamConn adapts an ssh.Channel to net.Conn.
type streamConn struct {
	ssh.Channel
	sshConnBase
	path string
}

func (c *streamConn) RemoteAddr() net.Addr { return sshAddr{"unix", c.path} }

// cmdConn adapts a remote command's stdio to net.Conn.
type cmdConn struct {
	sshConnBase
	sess   *ssh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	stderr syncBuffer
	cmd    string

	waitOnce sync.Once
	waitErr  error
}

func (c *cmdConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if err == io.EOF {
		// The command's stdout closed: it exited. Report why (non-zero exit,
		// stderr) rather than a bare EOF, so gRPC surfaces the real cause.
		if werr := c.Err(); werr != nil {
			return n, werr
		}
	}
	return n, err //nolint:wrapcheck // io.EOF and stream errors pass through
}

func (c *cmdConn) Write(p []byte) (int, error) {
	n, err := c.stdin.Write(p)
	if err != nil {
		return n, fmt.Errorf("sshx: write to %q: %w", c.cmd, err)
	}
	return n, nil
}

// Close ends the input side and tears down the session; the remote command
// sees EOF on its stdin and exits.
func (c *cmdConn) Close() error {
	c.stdin.Close()       //nolint:errcheck // closing input; session close is authoritative
	return c.sess.Close() //nolint:wrapcheck // thin pass-through
}

// Err waits for the command to finish (once) and returns a descriptive error
// if it failed, or nil on clean exit. It is how the transport layer recovers
// the remote's diagnostic after a failed connection: a net.Conn that carries
// a remote process exposes it via this method.
func (c *cmdConn) Err() error {
	c.waitOnce.Do(func() {
		werr := c.sess.Wait()
		if werr == nil {
			return
		}
		if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
			c.waitErr = fmt.Errorf("remote %q failed: %w: %s", c.cmd, werr, msg)
		} else {
			c.waitErr = fmt.Errorf("remote %q failed: %w", c.cmd, werr)
		}
	})
	return c.waitErr
}

func (c *cmdConn) RemoteAddr() net.Addr { return sshAddr{"exec", c.cmd} }

// syncBuffer is a tiny concurrency-safe buffer for capturing remote stderr,
// which the ssh library writes from its own goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // in-memory buffer never errors
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
