# Shevet

A single-binary Go system that lets one developer monitor and drive AI
execution agents (Claude Code and others) running inside tmux on machines they
own, through a local terminal dashboard.

> **Status:** design phase. This repository currently holds the architecture,
> the decision record, and the research that backs both. No application code yet.

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

## License

[MIT](LICENSE)
