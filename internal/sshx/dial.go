// Package sshx is Shevet's SSH-to-Host primitive: it resolves a Host's
// effective SSH configuration, opens one authenticated, host-key-verified
// connection, and hands out byte streams over it — either a direct channel to
// a remote unix socket (the fast path) or the stdio of a remote command (the
// fallback). It carries no Shevet protocol knowledge; the transport-selection
// policy and the `shevet _proxy` contract live in the client package.
//
// Auth and host-key material come from the developer's existing environment:
// ssh-agent, ~/.ssh identity files, and known_hosts — no new credentials
// (ADR-0003). A later slice (issue #5) reuses this package to upload the
// Server binary and start it over the same connection.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Sentinel errors let callers turn transport failures into actionable advice
// without matching on message text. Wrap, don't replace: the wrapped error
// keeps the underlying detail for logs.
var (
	// ErrAuth means the SSH server rejected every credential we offered.
	ErrAuth = errors.New("ssh authentication failed")

	// ErrHostKey means the Host's key was unknown or did not match the
	// pinned entry in known_hosts. Never downgraded to a warning.
	ErrHostKey = errors.New("ssh host key verification failed")

	// ErrUnsupportedProxy means the Host's ssh_config routes the connection
	// through a bastion (ProxyJump) or an external command (ProxyCommand),
	// which this transport does not yet implement. Reported up front rather
	// than dialing HostName:Port directly and failing obscurely before auth.
	ErrUnsupportedProxy = errors.New("ssh proxy configuration is not supported")
)

// handshakeTimeout bounds the TCP dial and SSH handshake so a black-holed
// Host fails fast instead of hanging the dashboard startup.
const handshakeTimeout = 15 * time.Second

// Conn is one live SSH connection to a Host. It is safe for concurrent use:
// each Dial* call opens an independent channel on the multiplexed connection.
// Always Close it.
type Conn struct {
	client *ssh.Client
	cfg    *Config
}

// Dial resolves cfg's credentials from the local environment, opens the SSH
// connection, and verifies the Host key. It returns ErrAuth or ErrHostKey
// (wrapped) for the two failures a user can act on; other failures (network,
// protocol) are returned as-is.
func Dial(ctx context.Context, cfg *Config) (*Conn, error) {
	if err := cfg.checkDirectlyReachable(); err != nil {
		return nil, err
	}

	methods, closeAuth := authMethods(cfg)
	defer closeAuth()

	hostKey, err := hostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}

	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         handshakeTimeout,
	}

	dialer := net.Dialer{Timeout: handshakeTimeout}
	tcp, err := dialer.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("sshx: connect to %s: %w", cfg.addr(), err)
	}

	// NewClientConn has no context; the handshake is bounded by ClientConfig
	// .Timeout, and a canceled ctx closes the socket to unblock it.
	stop := context.AfterFunc(ctx, func() { tcp.Close() }) //nolint:errcheck // best-effort unblock
	sshConn, chans, reqs, err := ssh.NewClientConn(tcp, cfg.addr(), clientCfg)
	stop()
	if err != nil {
		tcp.Close() //nolint:errcheck // handshake already failed
		return nil, classifyHandshake(cfg, err)
	}

	return &Conn{client: ssh.NewClient(sshConn, chans, reqs), cfg: cfg}, nil
}

// Close tears down the SSH connection and every channel opened on it.
func (c *Conn) Close() error {
	return c.client.Close() //nolint:wrapcheck // thin pass-through
}

// classifyHandshake maps a raw handshake failure to a sentinel where we can.
func classifyHandshake(cfg *Config, err error) error {
	switch {
	case errors.Is(err, ErrHostKey), errors.Is(err, ErrAuth):
		return err // our host-key callback already classified it
	case strings.Contains(err.Error(), "unable to authenticate"):
		return fmt.Errorf("%w for %s@%s: %v", ErrAuth, cfg.User, cfg.HostName, err)
	default:
		return fmt.Errorf("sshx: ssh handshake with %s: %w", cfg.addr(), err)
	}
}

// authMethods assembles public-key auth from the ssh-agent and any usable
// identity files. It returns a closer for the agent socket, which must stay
// open across the handshake because the agent signs there. "No keys" is not an
// error: it surfaces later as ErrAuth from the server, with the full list of
// methods tried — a more honest message than guessing up front.
func authMethods(cfg *Config) ([]ssh.AuthMethod, func()) {
	agentSigners, closeAgent := loadAgentSigners()
	fileSigners := loadIdentityFiles(cfg.IdentityFiles)

	// IdentitiesOnly=yes means "don't offer agent keys the user didn't list".
	// When we could load the listed keys from disk we honor it strictly; when
	// the listed keys are encrypted (loadable only via the agent) we keep the
	// agent keys rather than lock the user out — documented best-effort.
	if cfg.IdentitiesOnly && len(fileSigners) > 0 {
		agentSigners = nil
	}

	signers := dedupeSigners(append(fileSigners, agentSigners...))
	if len(signers) == 0 {
		return nil, closeAgent
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, closeAgent
}

// loadAgentSigners returns the signers held by the running ssh-agent, or nil
// (and a no-op closer) when no agent is reachable. The agent connection stays
// open via the returned closer because signing happens during the handshake.
func loadAgentSigners() ([]ssh.Signer, func()) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, func() {}
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, func() {}
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		conn.Close() //nolint:errcheck // best-effort
		return nil, func() {}
	}
	return signers, func() { conn.Close() } //nolint:errcheck // best-effort
}

// loadIdentityFiles parses each existing, unencrypted private key. Missing
// files and passphrase-protected keys are skipped silently: the former are
// just config defaults that don't exist, the latter are expected to be served
// by the agent.
func loadIdentityFiles(paths []string) []ssh.Signer {
	var signers []ssh.Signer
	for _, path := range paths {
		pem, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			// A PassphraseMissingError (or any parse failure) means we cannot
			// use this file directly; the agent may still hold the key.
			continue
		}
		signers = append(signers, signer)
	}
	return signers
}

// dedupeSigners drops signers whose public key already appears earlier, so a
// key present in both a file and the agent is offered once.
func dedupeSigners(signers []ssh.Signer) []ssh.Signer {
	seen := make(map[string]bool, len(signers))
	out := signers[:0]
	for _, s := range signers {
		fp := string(s.PublicKey().Marshal())
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, s)
	}
	return out
}

// hostKeyCallback builds the host-key verifier from cfg. When strict checking
// is off it accepts any key; otherwise it verifies against the existing
// known_hosts files and turns an unknown or changed key into ErrHostKey.
func hostKeyCallback(cfg *Config) (ssh.HostKeyCallback, error) {
	if !cfg.strict() {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // user set StrictHostKeyChecking=no
	}

	files := existingFiles(cfg.KnownHostsFiles)
	if len(files) == 0 {
		// Strict, but nowhere to check: refuse rather than trust blindly.
		return func(string, net.Addr, ssh.PublicKey) error {
			return fmt.Errorf("%w: no known_hosts file to verify %s against", ErrHostKey, cfg.HostName)
		}, nil
	}

	db, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("sshx: load known_hosts %v: %w", files, err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := db(hostname, remote, key); err != nil {
			return classifyHostKey(cfg.HostName, err)
		}
		return nil
	}, nil
}

// classifyHostKey wraps a knownhosts failure as ErrHostKey with advice keyed
// to whether the key is unknown (first contact) or changed (possible MITM).
func classifyHostKey(host string, err error) error {
	var keyErr *knownhosts.KeyError
	switch {
	case errors.As(err, &keyErr) && len(keyErr.Want) > 0:
		return fmt.Errorf("%w: key for %s changed — if this is expected, fix known_hosts; otherwise investigate", ErrHostKey, host)
	case errors.As(err, &keyErr):
		return fmt.Errorf("%w: %s is not in known_hosts — connect once with `ssh %s` to record its key", ErrHostKey, host, host)
	default:
		return fmt.Errorf("%w: %v", ErrHostKey, err)
	}
}

// existingFiles keeps only the paths that currently exist, since knownhosts
// .New fails if any file is missing but ssh -G lists defaults that may not be.
func existingFiles(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
