package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/irodion/shevet/internal/version"
)

func runVersion(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("shevet version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}

	fmt.Fprintf(stdout, "shevet %s (%s)\n", version.String(), version.GoVersion())
	return exitOK
}
