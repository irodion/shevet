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
| `read-raw <count> <path>` | byte count, then a file path | capture exactly `count` bytes of input **verbatim** and write them to `path` |
| `sleep <duration>` | Go duration (`200ms`, `3s`) | wall-clock delay (discouraged; see above) |
| `exit <code>` | integer | terminate with the exit code (0–255) |

### `read-raw`: byte-exact input capture

`read-raw` is the input-side counterpart to `await-line`. Where `await-line`
reads a cooked line and discards it, `read-raw` puts the pane's pty into **raw
mode** (no line discipline: control bytes, CR/LF, and NUL arrive as data, not
as signals or translations) and captures exactly `count` bytes to a file the
driving test then reads back. It is how the input slice proves a byte-diverse
corpus — printable UTF-8, control bytes, Cyrillic/CJK/emoji — arrives
byte-identically end to end.

It brackets the capture with two markers on stdout so a driver can sequence
its injection without a wall-clock race (both are `package scriptedagent`
constants):

- `shevet-read-raw-ready` (`ReadRawReady`) — printed **after** raw mode is
  active. Wait for it on the rendered screen before injecting; only then are
  the bytes guaranteed to arrive un-cooked.
- `shevet-read-raw-done` (`ReadRawDone`) — printed once all `count` bytes are
  captured and the file is written. Wait for it, then read the file.

`read-raw` needs a real terminal (the pty of a tmux pane); it errors if stdin
is a pipe. `path` runs to the end of the line, so it may contain spaces.

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
h.Tmux.SendLine(t, pane, "y")            // the deterministic "advance"
h.Tmux.WaitForContent(t, pane, "migration applied")
code := h.WaitForAgentExit(t, pane)      // from the exit-marker line
```

## The harness around it

Two packages, split so the tmux sandbox stays a leaf any layer can import
(the control-mode slice's white-box Server tests included):

- **`internal/tmuxtest`** — the hermetic sandbox: `Start(t)` boots a private
  tmux server (private socket, `-f /dev/null`, `TMUX` scrubbed) with *stock
  options* — the Server under test must see the same tmux semantics a
  user's tmux has. Teardown kills only the sandbox; a developer's own tmux
  is never touched. Skips when tmux is absent; CI installs it.
- **`internal/harness`** — the Level-1 composition:
  - `Start(t)`: sandbox + in-process Shevet Server (synchronous `Listen`,
    race-detector visible) + the real typed gRPC client from
    `internal/client`. The process boundary itself is covered separately by
    the `e2e` smoke tests.
  - `StartAgent(t, name, script)`: a scripted agent in a fresh window,
    returning the pane id (`%N`). The window command prints an exit marker
    and parks after the agent ends, so `WaitForAgentExit` reads the code
    from pane content — portable across tmux versions whose
    `pane_dead_status` behavior differs.
