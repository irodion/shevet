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
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// op is a script instruction. New ops are added as later slices need them
// (block-with-question, post-hook, emit-sixel, shevet-report, ...).
type op int

const (
	opPrint     op = iota // print <text>: write text + newline to stdout
	opPrompt              // prompt <text>: write text, no newline (a waiting prompt)
	opAwaitLine           // await-line: block until a line arrives on stdin
	opSleep               // sleep <duration>: explicit wall-clock delay
	opExit                // exit <code>: stop with the given exit code
)

// Step is one parsed script instruction.
type Step struct {
	op   op
	text string        // opPrint, opPrompt
	dur  time.Duration // opSleep
	code int           // opExit
}

// Script is a parsed scripted-agent program.
type Script struct {
	steps []Step
}

// Parse reads the line-based script vocabulary. Blank lines and lines
// starting with '#' are ignored. It rejects unknown ops and malformed
// arguments so a typo fails the test loudly instead of desynchronizing it.
func Parse(r io.Reader) (*Script, error) {
	var steps []Step

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

func parseStep(verb, arg string) (Step, error) {
	switch verb {
	case "print":
		return Step{op: opPrint, text: arg}, nil
	case "prompt":
		return Step{op: opPrompt, text: arg}, nil
	}

	// For the non-text ops, trailing whitespace is insignificant.
	arg = strings.TrimSpace(arg)
	switch verb {
	case "await-line":
		if arg != "" {
			return Step{}, fmt.Errorf("await-line takes no argument, got %q", arg)
		}
		return Step{op: opAwaitLine}, nil
	case "sleep":
		dur, err := time.ParseDuration(arg)
		if err != nil {
			return Step{}, fmt.Errorf("sleep: %w", err)
		}
		return Step{op: opSleep, dur: dur}, nil
	case "exit":
		code, err := strconv.Atoi(arg)
		if err != nil {
			return Step{}, fmt.Errorf("exit: %w", err)
		}
		return Step{op: opExit, code: code}, nil
	default:
		return Step{}, fmt.Errorf("unknown op %q", verb)
	}
}

// Run executes the script against the given streams and returns the exit
// code the process should terminate with: the argument of the first `exit`
// step, or 0 when the script ends without one. A read failure on stdin
// (e.g. the driving test went away mid `await-line`) returns an error.
func (s *Script) Run(stdin io.Reader, stdout io.Writer) (int, error) {
	in := bufio.NewReader(stdin)

	for _, step := range s.steps {
		switch step.op {
		case opPrint:
			if _, err := fmt.Fprintln(stdout, step.text); err != nil {
				return 1, fmt.Errorf("print: %w", err)
			}
		case opPrompt:
			if _, err := io.WriteString(stdout, step.text); err != nil {
				return 1, fmt.Errorf("prompt: %w", err)
			}
		case opAwaitLine:
			if _, err := in.ReadString('\n'); err != nil {
				return 1, fmt.Errorf("await-line: %w", err)
			}
		case opSleep:
			time.Sleep(step.dur)
		case opExit:
			return step.code, nil
		}
	}
	return 0, nil
}
