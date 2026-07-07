# The scripted-agent fixture

`shevet _agent --script <file>` is a deterministic fake Agent: a hidden
subcommand of the shevet binary that executes a small declarative script.
It exists so tests (and later `shevet demo`) control an Agent's output and
timing *exactly* — no real agent, no wall-clock races.

It is one artifact with several consumers — the e2e harness
(`internal/harness`), the future demo mode (issue #39), and README
recordings — so treat the script vocabulary as a small versioned interface:
extend it additively and keep this document current.

## Determinism contract

A script only advances through explicit steps. The synchronization primitive
is `await-line`: the agent parks until a full line arrives on stdin. Inside a
tmux pane that means the driving test decides when the agent proceeds (the
harness's `SendLine`), so assertions never race the fixture. `sleep` exists
for the later quiescence/Status heuristics tests and is otherwise discouraged.

## Vocabulary (v1)

One instruction per line. Blank lines and lines starting with `#` are
ignored. Unknown ops and malformed arguments are parse errors — a typo fails
loudly instead of desynchronizing a test.

| Op | Argument | Effect |
|----|----------|--------|
| `print <text>` | text to end of line | write text + newline to stdout |
| `prompt <text>` | text to end of line | write text *without* newline (renders as a waiting prompt) |
| `await-line` | none | block until a line arrives on stdin |
| `sleep <duration>` | Go duration (`200ms`, `3s`) | wall-clock delay (discouraged; see above) |
| `exit <code>` | integer | terminate with the exit code (0–255) |

A script that ends without `exit` terminates with code 0. If stdin closes
while parked on `await-line` (the driver went away), the agent exits
non-zero with an error on stderr.

Planned additions land with their slices: `block-with-question` /
structured options (#33/#34), hook posting (#18), `emit-sixel` (#25),
`shevet-report`/`ask`/`done` (#40).

## Example

```
# Simulates: work, ask for confirmation, finish successfully.
print compiling module a
print compiling module b
prompt Apply migration? [y/n]
await-line
print migration applied
exit 0
```

Driving it from a test via the harness:

```go
h := harness.Start(t)
pane := h.StartAgent(t, "agent-a", script)

h.Tmux.WaitForContent(t, pane, "Apply migration? [y/n]")
h.Tmux.SendLine(t, pane, "y")           // the deterministic "advance"
h.Tmux.WaitForContent(t, pane, "migration applied")
code := h.Tmux.WaitForExit(t, pane)      // remain-on-exit keeps the status
```

## The harness around it

`internal/harness` composes the three parties of every Level-1 scenario:

- **`StartTmux(t)`** — a hermetic tmux server on a private socket
  (`-f /dev/null`, `TMUX` scrubbed, `remain-on-exit` for exit-status
  assertions). Teardown kills only the sandbox server; a developer's own
  tmux is never touched. Skips when tmux is absent; CI installs it.
- **`Start(t)`** — sandbox + in-process Shevet Server (synchronous `Listen`,
  race-detector visible) + the real typed gRPC client from
  `internal/client`. The process boundary itself is covered separately by
  the `e2e` smoke tests.
- **`StartAgent(t, name, script)`** — a scripted agent in a fresh window,
  returning the pane id (`%N`) to target.
