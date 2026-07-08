package e2e

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/creack/pty"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/sshx"
	"github.com/irodion/shevet/internal/testutil"
)

// TestSSH_StreamLocalReachesServer is the acceptance path: `connect` reaches a
// running Server through a real sshd over the direct-streamlocal channel,
// honoring a config-supplied identity and a pinned host key.
func TestSSH_StreamLocalReachesServer(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	serve := startServe(t, bin, socket)
	srv := startSSHD(t, true /* streamlocal */)

	logs := connectVia(t, srv, bin, socket, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  srv.UserKey,
		knownHostLine: srv.hostPubLine,
		strict:        true,
	})
	if !strings.Contains(logs, "streamlocal channel open") {
		t.Errorf("expected the streamlocal transport to engage; transport log:\n%s", logs)
	}

	// Acceptance: no TCP listener carries Shevet traffic — the Server is
	// reachable purely over its unix socket through the SSH channel.
	assertNoTCPListeners(t, serve.Process.Pid)
}

// TestSSH_ProxyFallbackReachesServer forces the fallback by disabling
// streamlocal on the Host; the same Server is reached through `shevet _proxy`.
func TestSSH_ProxyFallbackReachesServer(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	startServe(t, bin, socket)
	srv := startSSHD(t, false /* streamlocal disabled */)

	logs := connectVia(t, srv, bin, socket, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  srv.UserKey,
		knownHostLine: srv.hostPubLine,
		strict:        true,
	})
	if !strings.Contains(logs, "proxy fallback") {
		t.Errorf("expected the proxy fallback to engage; transport log:\n%s", logs)
	}
}

// TestSSH_AgentAuth proves agent authentication is honored: no IdentityFile is
// configured, so the only credential is the key loaded into a private agent.
func TestSSH_AgentAuth(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	startServe(t, bin, socket)
	srv := startSSHD(t, true)

	startAgentWithKey(t, srv.UserKey)
	connectVia(t, srv, bin, socket, clientConfigOptions{
		alias:         "host-under-test",
		knownHostLine: srv.hostPubLine, // no identityFile: agent must supply the key
		strict:        true,
	})
}

// TestSSH_UnknownHostKeyRejected asserts an unknown host key is an error, not a
// shrug — strict checking with an empty known_hosts must refuse.
func TestSSH_UnknownHostKeyRejected(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	startServe(t, bin, socket)
	srv := startSSHD(t, true)

	t.Setenv("SSH_AUTH_SOCK", "")
	configFile := writeClientConfig(t, srv, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  srv.UserKey,
		knownHostLine: "", // never seen this host
		strict:        true,
	})
	_, err := client.DialSSH(context.Background(), "host-under-test", client.SSHOptions{ConfigFile: configFile, RemoteSocket: socket})
	if !errors.Is(err, sshx.ErrHostKey) {
		t.Fatalf("DialSSH error = %v, want sshx.ErrHostKey", err)
	}
}

// TestSSH_HostKeyMismatchRejected asserts a changed host key is refused even
// when a known_hosts entry exists.
func TestSSH_HostKeyMismatchRejected(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	startServe(t, bin, socket)
	srv := startSSHD(t, true)

	t.Setenv("SSH_AUTH_SOCK", "")
	// A syntactically valid entry for the right host:port but the wrong key.
	wrongKey := knownHostsLine(t, srv.Port, srv.UserKey+".pub")
	configFile := writeClientConfig(t, srv, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  srv.UserKey,
		knownHostLine: wrongKey,
		strict:        true,
	})
	_, err := client.DialSSH(context.Background(), "host-under-test", client.SSHOptions{ConfigFile: configFile, RemoteSocket: socket})
	if !errors.Is(err, sshx.ErrHostKey) {
		t.Fatalf("DialSSH error = %v, want sshx.ErrHostKey", err)
	}
}

// TestSSH_AuthFailure asserts a rejected credential surfaces as ErrAuth, with
// the host key verified first so the failure is unambiguously authentication.
func TestSSH_AuthFailure(t *testing.T) {
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	startServe(t, bin, socket)
	srv := startSSHD(t, true)

	// A valid key the Host does not trust.
	strangerKey := srv.UserKey + "_stranger"
	keygen(t, strangerKey)

	t.Setenv("SSH_AUTH_SOCK", "")
	configFile := writeClientConfig(t, srv, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  strangerKey,
		knownHostLine: srv.hostPubLine,
		strict:        true,
	})
	_, err := client.DialSSH(context.Background(), "host-under-test", client.SSHOptions{ConfigFile: configFile, RemoteSocket: socket})
	if !errors.Is(err, sshx.ErrAuth) {
		t.Fatalf("DialSSH error = %v, want sshx.ErrAuth", err)
	}
}

// TestSSH_NoServerIsActionable asserts that when no Server socket is present,
// the failure names the likely cause rather than a bare transport error.
func TestSSH_NoServerIsActionable(t *testing.T) {
	bin := testutil.BuildBinary(t)
	srv := startSSHD(t, true)

	t.Setenv("SSH_AUTH_SOCK", "")
	configFile := writeClientConfig(t, srv, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  srv.UserKey,
		knownHostLine: srv.hostPubLine,
		strict:        true,
	})
	// A socket path that was never created (no Server ran).
	absent := testutil.SocketPath(t)
	_, err := client.DialSSH(context.Background(), "host-under-test", client.SSHOptions{
		ConfigFile:    configFile,
		RemoteSocket:  absent,
		RemoteCommand: []string{bin, "_proxy"},
	})
	if err == nil {
		t.Fatal("expected an error reaching an absent Server, got nil")
	}
	if !strings.Contains(err.Error(), "Server running") {
		t.Errorf("error %q does not explain the missing Server", err)
	}
}

// TestSSH_ConnectBinaryEndToEnd drives the real `shevet connect <host>` binary
// over a PTY against a real sshd and Server — the whole CLI path: flag
// parsing, SSH transport, and the dashboard rendering the (empty) Herd. It
// also confirms the Client process opens no TCP listener of its own.
func TestSSH_ConnectBinaryEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY test is unix-only")
	}
	bin := testutil.BuildBinary(t)
	socket := testutil.SocketPath(t)
	startServe(t, bin, socket)
	srv := startSSHD(t, true)

	t.Setenv("SSH_AUTH_SOCK", "")
	configFile := writeClientConfig(t, srv, clientConfigOptions{
		alias:         "host-under-test",
		identityFile:  srv.UserKey,
		knownHostLine: srv.hostPubLine,
		strict:        true,
	})

	connect := exec.CommandContext(t.Context(), bin,
		"connect", "host-under-test",
		"--ssh-config", configFile,
		"--remote-socket", socket,
		"--log-level", "debug",
	)
	connect.Env = append(os.Environ(), "TERM=xterm-256color")

	// stdout/stdin ride the PTY (the dashboard needs a TTY); stderr, carrying
	// the transport log, is captured separately so it doesn't corrupt the
	// rendered screen we assert on.
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	defer ptmx.Close() //nolint:errcheck
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("set pty size: %v", err)
	}
	connect.Stdin, connect.Stdout = tty, tty
	var stderr testutil.SyncBuffer
	connect.Stderr = &stderr
	if err := connect.Start(); err != nil {
		t.Fatalf("start connect: %v", err)
	}
	tty.Close() //nolint:errcheck // the child owns it now
	t.Cleanup(func() {
		if connect.ProcessState == nil {
			connect.Process.Kill() //nolint:errcheck // last-resort cleanup
			connect.Wait()         //nolint:errcheck
		}
	})

	snapshot := ptySnapshot(ptmx)

	testutil.Eventually(t, `dashboard to render "0 Panes" over SSH`, func() (bool, string) {
		s := snapshot()
		return strings.Contains(s, "0 Panes"), fmt.Sprintf("stdout=%q stderr=%q", s, stderr.String())
	})

	if !strings.Contains(stderr.String(), "streamlocal channel open") {
		t.Errorf("expected streamlocal transport; connect stderr:\n%s", stderr.String())
	}
	// The Client, like the Server, exposes no TCP listener for its traffic.
	assertNoTCPListeners(t, connect.Process.Pid)

	if _, err := ptmx.WriteString("q"); err != nil {
		t.Fatalf("send quit key: %v", err)
	}
	if err := waitFor(connect, testutil.WaitTimeout); err != nil {
		t.Fatalf("connect did not exit cleanly after quit: %v\nstderr:\n%s", err, stderr.String())
	}
}

// connectVia writes an ssh_config for the server, dials the Server over SSH,
// and asserts the Herd is reachable (an empty Herd is success — the Server has
// no tmux session). It returns the transport log for path assertions.
func connectVia(t *testing.T, srv *sshdServer, bin, socket string, opts clientConfigOptions) string {
	t.Helper()

	configFile := writeClientConfig(t, srv, opts)
	var log testutil.SyncBuffer
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := client.DialSSH(context.Background(), opts.alias, client.SSHOptions{
		ConfigFile:    configFile,
		RemoteSocket:  socket,
		RemoteCommand: []string{bin, "_proxy"},
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("DialSSH: %v\ntransport log:\n%s", err, log.String())
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	panes, err := c.ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes over SSH: %v", err)
	}
	if len(panes) != 0 {
		t.Errorf("expected an empty Herd, got %d panes", len(panes))
	}
	return log.String()
}

// startAgentWithKey runs a private ssh-agent, loads key into it, and points
// SSH_AUTH_SOCK at it for the test.
func startAgentWithKey(t *testing.T, key string) {
	t.Helper()

	out, err := exec.Command("ssh-agent", "-s").Output()
	if err != nil {
		t.Skipf("ssh-agent unavailable: %v", err)
	}
	sock, pid := parseAgentEnv(string(out))
	if sock == "" || pid == "" {
		t.Skipf("could not parse ssh-agent output: %q", out)
	}
	t.Cleanup(func() { exec.Command("kill", pid).Run() }) //nolint:errcheck // agent teardown

	add := exec.Command("ssh-add", key)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)
}

// parseAgentEnv extracts the socket path and pid from `ssh-agent -s` output,
// which is a shell snippet of "NAME=value; export NAME;" lines.
func parseAgentEnv(sh string) (sock, pid string) {
	for _, line := range strings.Split(sh, "\n") {
		if v, ok := cutEnv(line, "SSH_AUTH_SOCK="); ok {
			sock = v
		}
		if v, ok := cutEnv(line, "SSH_AGENT_PID="); ok {
			pid = v
		}
	}
	return sock, pid
}

func cutEnv(line, prefix string) (string, bool) {
	i := strings.Index(line, prefix)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(prefix):]
	val, _, _ := strings.Cut(rest, ";")
	return val, true
}

// assertNoTCPListeners fails if the given process holds any listening TCP
// socket, enforcing the "zero open TCP ports for Shevet traffic" rule. It
// skips gracefully when no socket-inspection tool is available.
func assertNoTCPListeners(t *testing.T, pid int) {
	t.Helper()

	lsof, err := exec.LookPath("lsof")
	if err != nil {
		t.Logf("lsof not available; skipping the no-TCP-listener assertion")
		return
	}
	// lsof exits non-zero when nothing matches, which is exactly the pass
	// case, so we inspect the output rather than the exit code.
	out, _ := exec.Command(lsof, "-nP", "-a", "-p", fmt.Sprint(pid), "-iTCP", "-sTCP:LISTEN").Output()
	if listeners := strings.TrimSpace(string(out)); listeners != "" {
		t.Errorf("process %d holds TCP listeners (want none):\n%s", pid, listeners)
	}
}
