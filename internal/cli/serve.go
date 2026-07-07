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
	logLevel := slog.LevelInfo
	flags.TextVar(&logLevel, "log-level", slog.LevelInfo, "log level: debug, info, warn, error")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "shevet serve: unexpected argument %q\n", flags.Arg(0))
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

	srv := server.New(server.Options{SocketPath: socketPath}, log)
	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "error", err)
		return exitError
	}
	return exitOK
}
