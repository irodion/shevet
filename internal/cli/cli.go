// Package cli wires the shevet command line: one binary, roles selected by
// subcommand — never by build flags and never by a --mode flag
// (ARCHITECTURE.md §2, CONTEXT.md).
package cli

import (
	"context"
	"fmt"
	"io"
)

// Exit codes follow sysexits-flavored convention: 0 success, 1 runtime
// failure, 2 usage error.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const rootUsage = `shevet — monitor and drive AI Agents running in tmux

Usage:
  shevet <command> [flags]

Commands:
  serve     run the Server daemon on this Host
  connect   open the Client dashboard against a Server
  version   print version information

Use "shevet <command> -h" for command flags.
`

// Run executes the shevet command line and returns the process exit code.
// ctx carries cancellation (signals are wired by main); output goes to the
// provided writers so tests can capture it.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, rootUsage)
		return exitUsage
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return runServe(ctx, rest, stdout, stderr)
	case "connect":
		return runConnect(ctx, rest, stdout, stderr)
	case "version":
		return runVersion(rest, stdout, stderr)
	case "_proxy":
		// Hidden: the stdio<->socket pump used as the SSH tunnel fallback.
		// Deliberately absent from rootUsage.
		return runProxy(ctx, rest, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, rootUsage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "shevet: unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, rootUsage)
		return exitUsage
	}
}
