package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/testutil"
)

// sshdServer is a real OpenSSH daemon bound to loopback for the duration of a
// test — the containerless equivalent of the Level-2 sshd the acceptance
// calls for (issue #10). It authenticates a generated key, pins a generated
// host key, and can toggle streamlocal forwarding to exercise both transports.
type sshdServer struct {
	Port        string // ephemeral loopback port
	UserKey     string // path to the client private key it accepts
	hostPubLine string // known_hosts entry for this server
}

// startSSHD launches sshd on a loopback port and returns once it accepts
// connections. streamLocal controls AllowStreamLocalForwarding, selecting
// which client transport (direct channel vs. proxy) the Host offers. The test
// is skipped when no sshd binary is available.
func startSSHD(t *testing.T, streamLocal bool) *sshdServer {
	t.Helper()

	sshd := findSSHD(t)
	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host_ed25519")
	userKey := filepath.Join(dir, "id_ed25519")
	authKeys := filepath.Join(dir, "authorized_keys")

	keygen(t, hostKey)
	keygen(t, userKey)
	if err := os.WriteFile(authKeys, readFile(t, userKey+".pub"), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	port := freePort(t)
	cfgPath := filepath.Join(dir, "sshd_config")
	streamLocalOpt := "no"
	if streamLocal {
		streamLocalOpt = "yes"
	}
	cfg := fmt.Sprintf(`Port %s
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
AllowStreamLocalForwarding %s
LogLevel VERBOSE
`, port, hostKey, filepath.Join(dir, "sshd.pid"), authKeys, streamLocalOpt)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write sshd_config: %v", err)
	}

	// sshd re-execs itself, so it needs an absolute binary path and config.
	logPath := filepath.Join(dir, "sshd.log")
	cmd := exec.CommandContext(t.Context(), sshd, "-D", "-f", cfgPath, "-E", logPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sshd: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck // last-resort cleanup; the Wait goroutine reaps
		if t.Failed() || t.Skipped() {
			if log, err := os.ReadFile(logPath); err == nil {
				t.Logf("sshd log:\n%s", log)
			}
		}
	})

	// Wait for the port, but bail to a skip if sshd exits first: a sandbox
	// that cannot run a user-space sshd (a CI quirk, not a Shevet bug) should
	// skip loudly, not fail the build.
	deadline := time.Now().Add(testutil.WaitTimeout)
	for {
		select {
		case err := <-exited:
			t.Skipf("sshd exited before accepting connections (%v); skipping the real-sshd test", err)
		default:
		}
		if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
			conn.Close() //nolint:errcheck
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sshd on 127.0.0.1:%s did not accept connections within %s", port, testutil.WaitTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}

	return &sshdServer{Port: port, UserKey: userKey, hostPubLine: knownHostsLine(t, port, hostKey+".pub")}
}

// clientConfigOptions shapes the fake ~ that `ssh -G` reads when shevet resolves
// the connection, letting a test exercise config, host-key, and auth paths.
type clientConfigOptions struct {
	alias         string // Host alias the client connects to
	identityFile  string // IdentityFile line; empty relies on the agent
	knownHostLine string // known_hosts content; empty means an empty file
	strict        bool   // StrictHostKeyChecking yes vs no
}

// writeClientConfig builds an ssh_config and known_hosts for the given server
// in a temp dir and returns the config path, which the test passes to DialSSH
// as SSHOptions.ConfigFile (ssh's -F). ssh resolves ~/.ssh/config against the
// passwd home, not $HOME, so an explicit config file is how a test injects
// one — the same seam `shevet connect --ssh-config` exposes.
func writeClientConfig(t *testing.T, srv *sshdServer, opts clientConfigOptions) string {
	t.Helper()

	dir := t.TempDir()
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(opts.knownHostLine), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	strict := "no"
	if opts.strict {
		strict = "yes"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Host %s\n", opts.alias)
	fmt.Fprintf(&b, "    HostName 127.0.0.1\n")
	fmt.Fprintf(&b, "    Port %s\n", srv.Port)
	fmt.Fprintf(&b, "    User %s\n", me.Username)
	fmt.Fprintf(&b, "    UserKnownHostsFile %s\n", knownHosts)
	fmt.Fprintf(&b, "    StrictHostKeyChecking %s\n", strict)
	if opts.identityFile != "" {
		fmt.Fprintf(&b, "    IdentityFile %s\n", opts.identityFile)
		fmt.Fprintf(&b, "    IdentitiesOnly yes\n")
	}
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write ssh config: %v", err)
	}
	return configPath
}

func findSSHD(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	if path, err := exec.LookPath("sshd"); err == nil {
		return path
	}
	t.Skip("sshd not found; skipping the real-sshd Level-2 test")
	return ""
}

func keygen(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen %s: %v\n%s", path, err, out)
	}
}

// knownHostsLine renders a "[127.0.0.1]:port keytype key" entry from a public
// key file, matching how the ssh library addresses a non-default port.
func knownHostsLine(t *testing.T, port, pubPath string) string {
	t.Helper()
	fields := strings.Fields(string(readFile(t, pubPath)))
	if len(fields) < 2 {
		t.Fatalf("malformed public key %s", pubPath)
	}
	return fmt.Sprintf("[127.0.0.1]:%s %s %s\n", port, fields[0], fields[1])
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer ln.Close() //nolint:errcheck
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split port: %v", err)
	}
	return port
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
