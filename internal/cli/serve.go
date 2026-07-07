package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/irodion/shevet/internal/paths"
	"github.com/irodion/shevet/internal/server"
)

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shevet serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "unix socket to listen on (default ~/.shevet/shevet.sock)")
	tmuxSession := flags.String("tmux-session", "", "tmux session whose panes form the Herd (empty serves an empty Herd)")
	tmuxSocket := flags.String("tmux-socket", "", "tmux server socket path (default: the user's default tmux server)")
	logLevel := slog.LevelInfo
	flags.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, error")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "shevet serve: unexpected argument %q\n", flags.Arg(0))
		return exitUsage
	}
	if *tmuxSocket != "" && *tmuxSession == "" {
		fmt.Fprintln(stderr, "shevet serve: --tmux-socket requires --tmux-session")
		return exitUsage
	}

	socketPath := *socket
	if socketPath == "" {
		var err error
		if socketPath, err = paths.DefaultSocket(); err != nil {
			fmt.Fprintf(stderr, "shevet serve: %v\n", err)
			return exitError
		}
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: logLevel}))

	opts := server.Options{SocketPath: socketPath}
	if *tmuxSession != "" {
		opts.Tmux = &server.TmuxOptions{Socket: *tmuxSocket, Session: *tmuxSession}
	}

	srv := server.New(opts, log)
	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "error", err)
		return exitError
	}
	return exitOK
}
