package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/tui"
)

func runConnect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shevet connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "connect to a local Server on this unix socket")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	switch {
	case flags.NArg() > 1:
		fmt.Fprintf(stderr, "shevet connect: expected at most one <host> argument, got %d\n", flags.NArg())
		return exitUsage
	case flags.NArg() == 1 && *socket != "":
		fmt.Fprintln(stderr, "shevet connect: <host> and --socket are mutually exclusive")
		return exitUsage
	case flags.NArg() == 1:
		// The SSH transport is a later slice (issue #10).
		fmt.Fprintf(stderr, "shevet connect: SSH transport to %q is not implemented yet; use --socket to reach a local Server\n", flags.Arg(0))
		return exitError
	case *socket == "":
		fmt.Fprintln(stderr, "shevet connect: a target is required: <host> or --socket <path>")
		return exitUsage
	}

	c, err := client.Dial(*socket)
	if err != nil {
		fmt.Fprintf(stderr, "shevet connect: %v\n", err)
		return exitError
	}
	defer c.Close() //nolint:errcheck // read-only connection teardown

	if err := tui.Run(ctx, c); err != nil {
		fmt.Fprintf(stderr, "shevet connect: %v\n", err)
		return exitError
	}
	return exitOK
}
