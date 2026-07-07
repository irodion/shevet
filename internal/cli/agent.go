package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/irodion/shevet/internal/scriptedagent"
)

// runAgent is the hidden scripted-agent fixture: a deterministic fake Agent
// driven by a declarative script (docs/testing/scripted-agent.md). The e2e
// harness runs it inside sandbox tmux panes; `shevet demo` reuses it later.
// Living in the shevet binary keeps it one artifact — no separate fixture
// build to distribute or drift.
func runAgent(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shevet _agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scriptPath := flags.String("script", "", "path to the agent script (required)")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *scriptPath == "" {
		fmt.Fprintln(stderr, "shevet _agent: --script is required")
		return exitUsage
	}

	f, err := os.Open(*scriptPath)
	if err != nil {
		fmt.Fprintf(stderr, "shevet _agent: %v\n", err)
		return exitError
	}
	defer f.Close() //nolint:errcheck // read-only file

	script, err := scriptedagent.Parse(f)
	if err != nil {
		fmt.Fprintf(stderr, "shevet _agent: %v\n", err)
		return exitUsage
	}

	code, err := script.Run(os.Stdin, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "shevet _agent: %v\n", err)
		return exitError
	}
	return code
}
