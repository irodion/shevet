# Shevet

A single-binary Go system that lets one developer monitor and drive AI
execution agents (Claude Code and others) running inside tmux on machines they
own, through a local terminal dashboard.

> **Status:** early development. The walking skeleton is in place — `shevet
> serve` and `shevet connect` talk gRPC over a local unix socket. The roadmap
> lives in the issue tracker (PRDs [#1](https://github.com/irodion/shevet/issues/1)–[#4](https://github.com/irodion/shevet/issues/4)).

## What it does

One binary, two roles selected at runtime:

- `shevet serve` — the **Server**, running on a Host. It attaches to tmux in
  control mode, runs a VT emulator per Pane, tracks each Agent's Status, and
  streams cell-level screen updates over a gRPC tunnel.
- `shevet connect <host>` — the **Client**, a Bubble Tea terminal dashboard on
  your laptop. It bootstraps and reaches the Server over SSH, renders the Pane
  grid, forwards your input, and raises OS notifications when an Agent needs you.

You watch several coding agents at once, jump into any one to steer it, and get
alerted the moment one finishes or blocks — all from a single terminal.

## Repository layout

| Path | Contents |
|------|----------|
| `ARCHITECTURE.md` | The authoritative detailed architecture. |
| `CONTEXT.md` | Domain glossary (Herd, Agent, Host, Server, Client, Pane, Status, Signal, Spawn, Adopt, Focus Lease). |
| `docs/adr/` | Architecture Decision Records — the hard/surprising trade-offs and why. |
| `docs/research/` | Supporting research, including the VT-emulator benchmark (`vtbench/`). |

Start with `ARCHITECTURE.md`, then read `docs/adr/` for the reasoning behind
each non-obvious choice.

## Development

Requires Go (see `go.mod`); protobuf tooling only when changing `.proto` files.

```sh
make build   # build ./bin/shevet (CGO-free)
make test    # all tests, race detector on
make lint    # gofmt + go vet + staticcheck
make cross   # verify every supported GOOS/GOARCH builds
```

Try the skeleton locally:

```sh
./bin/shevet serve --socket /tmp/shevet.sock &
./bin/shevet connect --socket /tmp/shevet.sock   # q to quit
```

## License

[MIT](LICENSE)
