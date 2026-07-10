package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/sshx"
	"github.com/irodion/shevet/internal/tui"
)

func runConnect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shevet connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "connect to a local Server on this unix socket")
	remoteSocket := flags.String("remote-socket", "", "Server socket path on the Host (default ~/.shevet/shevet.sock)")
	sshConfig := flags.String("ssh-config", "", "use this ssh_config instead of ~/.ssh/config (ssh -F)")
	logLevel := slog.LevelWarn
	flags.TextVar(&logLevel, "log-level", slog.LevelWarn, "transport log level: debug, info, warn, error")

	// The <host> is a positional argument, but people naturally write
	// `connect myhost --log-level debug`. Go's flag parser stops at the first
	// non-flag token, so pull a leading host out before parsing; flags may
	// still precede a bare host too.
	var host string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case host != "" && flags.NArg() > 0, host == "" && flags.NArg() > 1:
		fmt.Fprintln(stderr, "shevet connect: expected at most one <host> argument")
		return exitUsage
	case host == "" && flags.NArg() == 1:
		host = flags.Arg(0)
	}

	switch {
	case host != "" && *socket != "":
		fmt.Fprintln(stderr, "shevet connect: <host> and --socket are mutually exclusive")
		return exitUsage
	case host == "" && *socket == "":
		fmt.Fprintln(stderr, "shevet connect: a target is required: <host> or --socket <path>")
		return exitUsage
	case host == "" && *remoteSocket != "":
		fmt.Fprintln(stderr, "shevet connect: --remote-socket applies to a <host> target, not --socket")
		return exitUsage
	}

	if path, nonBoot := executableOnNonBootVolume(); nonBoot {
		fmt.Fprintf(stderr, "shevet connect: warning: %s (%s)\n", nonBootVolumeWarning, path)
	}

	c, err := dialTarget(ctx, host, *socket, client.SSHOptions{
		RemoteSocket: *remoteSocket,
		ConfigFile:   *sshConfig,
		Logger:       slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: logLevel})),
	})
	if err != nil {
		fmt.Fprintf(stderr, "shevet connect: %s\n", connectAdvice(err))
		return exitError
	}
	defer c.Close() //nolint:errcheck // read-only connection teardown

	// The Client keys every Pane by PaneRef (Host alias + pane id); this slice
	// dials one Host, so its alias scopes the whole Herd. Multi-Host dialing
	// arrives with the dashboard (#11); NewServers already enforces the alias
	// uniqueness that identity depends on.
	servers, err := tui.NewServers([]tui.Server{{Alias: hostAlias(host), Conn: tui.FromClient(c)}})
	if err != nil {
		fmt.Fprintf(stderr, "shevet connect: %v\n", err)
		return exitError
	}

	if err := tui.Run(ctx, servers); err != nil {
		fmt.Fprintf(stderr, "shevet connect: %v\n", err)
		return exitError
	}
	return exitOK
}

// hostAlias names the connection for PaneRef scoping: the SSH <host> as
// written, or "local" for a --socket target (which has no host name).
func hostAlias(host string) string {
	if host == "" {
		return "local"
	}
	return host
}

// dialTarget picks the transport from the parsed target: a remote Host over
// SSH (using sshOpts), or a local unix socket.
func dialTarget(ctx context.Context, host, socket string, sshOpts client.SSHOptions) (*client.Client, error) {
	if host != "" {
		return client.DialSSH(ctx, host, sshOpts)
	}
	return client.Dial(socket)
}

// connectAdvice turns a dial failure into a one-line message that says which
// of the three failure modes happened and what to do — authentication, host
// key, or an unreachable Server (ADR-0003, issue #10).
func connectAdvice(err error) string {
	switch {
	case errors.Is(err, sshx.ErrAuth):
		return fmt.Sprintf("%v\n  the Host rejected your SSH credentials; check `ssh <host>` works and your key is in the agent (ssh-add -l)", err)
	case errors.Is(err, sshx.ErrHostKey):
		return fmt.Sprintf("%v\n  refusing to connect with an unverified Host key", err)
	default:
		return err.Error()
	}
}
