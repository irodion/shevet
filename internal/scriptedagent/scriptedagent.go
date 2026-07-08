// Package scriptedagent implements the deterministic fake Agent used by the
// e2e harness (and, later, `shevet demo`). It executes a small declarative
// script — the "script vocabulary" documented in docs/testing/scripted-agent.md
// — so tests control an Agent's output and timing exactly.
//
// Determinism is the point: the only ways a script advances are explicit
// steps. The `await-line` step is the synchronization primitive — the agent
// blocks until the test injects a line of input — so tests never race the
// fixture on wall-clock time.
//
// The vocabulary is a versioned interface with multiple consumers (harness,
// demo mode, README recordings); extend it additively and update the doc.
package scriptedagent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
)

// ReadRawReady and ReadRawDone bracket a read-raw step on stdout: the agent
// prints Ready once its stdin is in raw mode (so a driver knows injected
// bytes will arrive un-cooked), and Done once it has captured every byte and
// written the file. Drivers wait for these on the rendered screen; both are
// terminated with CR+LF because raw mode disables output post-processing.
const (
	ReadRawReady = "shevet-read-raw-ready"
	ReadRawDone  = "shevet-read-raw-done"
)

// op is a script instruction. New ops are added as later slices need them
// (block-with-question, post-hook, emit-sixel, shevet-report, ...).
type op int

const (
	opPrint     op = iota // print <text>: write text + newline to stdout
	opPrompt              // prompt <text>: write text, no newline (a waiting prompt)
	opAwaitLine           // await-line: block until a line arrives on stdin
	opReadRaw             // read-raw <count> <path>: capture count raw stdin bytes to a file
	opSleep               // sleep <duration>: explicit wall-clock delay
	opExit                // exit <code>: stop with the given exit code
)

// step is one parsed script instruction.
type step struct {
	op    op
	text  string        // opPrint, opPrompt
	dur   time.Duration // opSleep
	code  int           // opExit
	count int           // opReadRaw: number of bytes to capture
	path  string        // opReadRaw: file to write the captured bytes to
}

// Script is a parsed scripted-agent program.
type Script struct {
	steps []step
}

// Parse reads the line-based script vocabulary. Blank lines and lines
// starting with '#' are ignored. It rejects unknown ops and malformed
// arguments so a typo fails the test loudly instead of desynchronizing it.
func Parse(r io.Reader) (*Script, error) {
	var steps []step

	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		// Only leading whitespace is insignificant: the argument runs to
		// the end of the line verbatim, trailing spaces included (a prompt
		// almost always wants one).
		line := strings.TrimLeft(scanner.Text(), " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		verb, arg, _ := strings.Cut(line, " ")
		step, err := parseStep(verb, arg)
		if err != nil {
			return nil, fmt.Errorf("script line %d: %w", lineNo, err)
		}
		steps = append(steps, step)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read script: %w", err)
	}
	return &Script{steps: steps}, nil
}

// parseStep parses one instruction. Text ops take their argument verbatim;
// for the others, surrounding whitespace is insignificant.
func parseStep(verb, arg string) (step, error) {
	switch verb {
	case "print":
		return step{op: opPrint, text: arg}, nil
	case "prompt":
		return step{op: opPrompt, text: arg}, nil
	case "await-line":
		if strings.TrimSpace(arg) != "" {
			return step{}, fmt.Errorf("await-line takes no argument, got %q", arg)
		}
		return step{op: opAwaitLine}, nil
	case "read-raw":
		countStr, path, ok := strings.Cut(strings.TrimLeft(arg, " \t"), " ")
		count, err := strconv.Atoi(countStr)
		if err != nil {
			return step{}, fmt.Errorf("read-raw: byte count: %w", err)
		}
		if count < 0 {
			return step{}, fmt.Errorf("read-raw: negative byte count %d", count)
		}
		if !ok || strings.TrimSpace(path) == "" {
			return step{}, errors.New("read-raw: want <count> <path>")
		}
		return step{op: opReadRaw, count: count, path: path}, nil
	case "sleep":
		dur, err := time.ParseDuration(strings.TrimSpace(arg))
		if err != nil {
			return step{}, fmt.Errorf("sleep: %w", err)
		}
		return step{op: opSleep, dur: dur}, nil
	case "exit":
		code, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil {
			return step{}, fmt.Errorf("exit: %w", err)
		}
		return step{op: opExit, code: code}, nil
	default:
		return step{}, fmt.Errorf("unknown op %q", verb)
	}
}

// Run executes the script against the given streams and returns the exit
// code the process should terminate with: the argument of the first `exit`
// step, or 0 when the script ends without one. On error — e.g. stdin closed
// mid `await-line` because the driving test went away — the code is
// meaningless; the caller owns the process exit code for failures.
func (s *Script) Run(stdin io.Reader, stdout io.Writer) (int, error) {
	in := bufio.NewReader(stdin)

	for _, st := range s.steps {
		switch st.op {
		case opPrint:
			if _, err := fmt.Fprintln(stdout, st.text); err != nil {
				return 0, fmt.Errorf("print: %w", err)
			}
		case opPrompt:
			if _, err := io.WriteString(stdout, st.text); err != nil {
				return 0, fmt.Errorf("prompt: %w", err)
			}
		case opAwaitLine:
			if _, err := in.ReadString('\n'); err != nil {
				return 0, fmt.Errorf("await-line: %w", err)
			}
		case opReadRaw:
			if err := readRaw(stdin, in, stdout, st.count, st.path); err != nil {
				return 0, fmt.Errorf("read-raw: %w", err)
			}
		case opSleep:
			time.Sleep(st.dur)
		case opExit:
			return st.code, nil
		}
	}
	return 0, nil
}

// readRaw captures exactly count bytes of input verbatim and writes them to
// path. It puts stdin's terminal into raw mode first, so control bytes and
// CR/LF reach the agent as data instead of being interpreted or translated by
// the line discipline — the byte-fidelity contract the input slice proves.
//
// Reads go through the same buffered reader the rest of the script uses, so a
// byte already buffered from an earlier step is never stranded; MakeRaw needs
// the underlying descriptor, which stdin (an *os.File in a real pane)
// provides. The Ready/Done markers let a driver sequence its injection
// against raw mode without a wall-clock race.
func readRaw(stdin io.Reader, in *bufio.Reader, stdout io.Writer, count int, path string) error {
	f, ok := stdin.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("stdin has no file descriptor (not a terminal)")
	}
	fd := f.Fd()
	if !term.IsTerminal(fd) {
		return errors.New("stdin is not a terminal; raw capture needs a pty")
	}

	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("enter raw mode: %w", err)
	}
	defer term.Restore(fd, old) //nolint:errcheck // best-effort restore before exit

	// Announce readiness only after raw mode is active. Raw mode disables
	// OPOST, so terminate the marker with CR+LF ourselves.
	if _, err := io.WriteString(stdout, ReadRawReady+"\r\n"); err != nil {
		return fmt.Errorf("write ready marker: %w", err)
	}

	buf := make([]byte, count)
	if _, err := io.ReadFull(in, buf); err != nil {
		return fmt.Errorf("read %d bytes: %w", count, err)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := io.WriteString(stdout, ReadRawDone+"\r\n"); err != nil {
		return fmt.Errorf("write done marker: %w", err)
	}
	return nil
}
