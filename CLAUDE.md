# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Shevet is a single Go binary with two runtime roles, selected by subcommand (never by build flags):

- `shevet serve` — the **Server**, a headless daemon on a Host. Attaches to tmux in control mode (`tmux -C`), runs a per-Pane VT emulator, derives each Agent's Status, and streams cell-level screen damage over gRPC.
- `shevet connect <host>` — the **Client**, a Bubble Tea v2 TUI on the developer's laptop. Bootstraps and reaches the Server over SSH, renders the Pane grid, forwards input, and raises OS notifications.

One developer, one or more Hosts they own. No multi-user identity, tenancy, or shared infrastructure. Hosts must be unix (tmux is load-bearing); the Client can target any Go platform including Windows. Note that `make cross`'s `PLATFORMS` only verifies the unix Host/Client matrix (linux and darwin) — a Windows Client build works but isn't part of that check.

## Commands

```sh
make build   # build ./bin/shevet for the current platform (CGO-free)
make test    # go test -race ./...  (all tests, race detector on)
make lint    # gofmt check + go vet + staticcheck (via `go tool`)
make cross   # verify every GOOS/GOARCH in PLATFORMS builds (use -j for parallel)
make proto   # regenerate gRPC/protobuf from proto/ (needs protoc + plugins)
```

Run a single test: `go test -race ./internal/server -run TestName`.

Try the skeleton locally:
```sh
./bin/shevet serve --socket /tmp/shevet.sock &
./bin/shevet connect --socket /tmp/shevet.sock   # q to quit
```

- **All builds are `CGO_ENABLED=0` by design** (ADR-0002 — pure-Go TUI, no Zig/CGO). Cross-compilation is a plain GOOS/GOARCH matrix.
- **tmux is required to run the e2e/harness tests** (CI installs it). `testutil.BuildBinary(t)` compiles a fresh binary inside e2e tests; PTY-based tests skip on non-unix.
- `make lint` deliberately fails if staticcheck matches zero packages, and excludes `docs/` (which has its own modules).

## Architecture

`ARCHITECTURE.md` is the authoritative design; `CONTEXT.md` is the domain glossary; `docs/adr/` records every non-obvious trade-off and why. **Read these before making design-affecting changes** — capitalized terms (Agent, Host, Pane, Server, Client, Status, Signal, Spawn, Adopt, Focus Lease) are used in their glossary sense throughout the code, and ARCHITECTURE.md cites ADRs by number for each decision.

### Data flow (Server → Client)

tmux `%output` bytes → per-Pane **VT emulator** (`internal/emu`, wraps `charmbracelet/x/vt`, ADR-0006) → **damage** (dirty cells) → per-Pane **coalescer** (flushes ≤ every 16ms via a one-shot timer, last-write-wins per cell, so idle Panes cost zero wakeups) → **wire** encoding → gRPC Matrix Render stream → Client's local **grid** → **tui** render.

Input flows the reverse way: Client keystrokes → Control Input stream → `internal/inject` splits batched bytes into tmux `send-keys` runs by type (literal text vs `-H` hex control bytes vs paste vs Server-encoded mouse). Input/resize are honored only from the **Focus Lease** holder (at most one Client per Pane; entering steals the lease).

### Package map (`internal/`)

| Package | Role |
|---|---|
| `cli` | Subcommand dispatch: `serve`, `connect`, `proxy` (hidden stdio↔socket tunnel), `agent`, `version`, boot-volume warning. Entry from `main.go`. |
| `server` | The Server: `service` (gRPC handlers), `pipeline` (damage coalescing), `registry` (Panes/Agents), `watcher`. |
| `client` | Client side of the `shevet.v1` API: dials the Server, consumes streams. |
| `tui` | Bubble Tea dashboard — `model`, `keys`, `render`. |
| `emu` | VT emulator behind Shevet's own `Emulator` interface (ADR-0001, ADR-0006). |
| `grid` | Shared screen vocabulary: `Cell`, `Attr` — used by both Server and Client. |
| `herd` | Domain types (`Pane`, Agent, Status) shared across roles — the glossary in code. |
| `wire` | Translates domain types (`herd`, `grid`) ↔ protobuf (`proto/shevet/v1`). |
| `inject` | Client raw bytes → tmux `send-keys` invocations. |
| `tmuxctl` | tmux control-mode (`tmux -C`) attachment: connection + notification parsing. |
| `paths` | On-Host filesystem conventions (the `~/.shevet` directory, socket, spool). |
| `harness` / `scriptedagent` / `tmuxtest` / `testutil` | Level-1 e2e harness: hermetic tmux sandbox + deterministic fake Agent + shared test helpers. |

### Protocol

gRPC + Protobuf over an SSH-tunneled unix socket (`~/.shevet/shevet.sock`, mode 0600, no TCP by default — ADR-0003). Three long-lived streams — **Matrix Render** (Server→Client damage/snapshots), **Control Input** (Client→Server keys/paste/mouse/resize/focus), **Telemetry** (Server→Client status/focus/host-health) — plus unary RPCs (`ListPanes`, `Spawn`, `Adopt`, `Release`, `Kill`, `FetchHistory`, `ServerInfo`). Protobuf fields are additive; `ServerInfo` gates incompatible changes. Regenerate with `make proto` after editing `proto/shevet/v1/shevet.proto` — never hand-edit the generated `*.pb.go`.

## Conventions worth knowing

- **Never run regex over raw byte streams** (ADR-0004). Screen Signals for Status detection evaluate patterns against *rendered rows near the cursor*, never raw `%output` bytes.
- **Spawn vs Adopt is a hard ownership boundary** (CONTEXT.md): Shevet owns a Spawned Agent cradle-to-grave and may `Kill` it; an Adopted Agent is only observed — `Release` stops watching but Shevet never terminates its process.
- On reconnect the Server reseeds Panes from **emulator memory** (`GridSnapshot`), not `capture-pane` (ADR-0001). `capture-pane` is only used for degraded reconstruction after a full Server restart (§5.3).
- Prefer the domain vocabulary from `CONTEXT.md` in names and comments; the glossary lists disallowed synonyms (e.g. avoid "node"/"cluster" for Host, "lock" for Focus Lease).
