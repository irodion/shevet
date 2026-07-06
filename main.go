// Command shevet is a single binary with two runtime roles: `shevet serve`
// runs the Server daemon on a Host; `shevet connect` opens the Client
// dashboard. Roles are selected by subcommand, never by build flags.
//
// See ARCHITECTURE.md for the design and CONTEXT.md for vocabulary.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/irodion/shevet/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
