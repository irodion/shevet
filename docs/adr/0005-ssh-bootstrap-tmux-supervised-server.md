# Client bootstraps the Server over SSH; the Server runs inside the Shevet tmux session

There is no manual install step per Host: `shevet connect <host>` checks the Server binary's presence and version over SSH, uploads the correct GOOS/GOARCH artifact when missing or stale, and starts the Server **inside the Shevet-owned tmux session**. Teardown is `tmux kill-session -t shevet` (exposed as `shevet host teardown`), which kills the Server, its respawn wrapper, and all Spawned Agents — and nothing else. It must **never** be `tmux kill-server`: Adopted Agents live in other sessions of the same tmux server, and Adopt semantics (see CONTEXT.md) forbid Shevet from terminating them. Reboot persistence is opt-in (`shevet host enable-boot` writes a systemd user unit / launchd plist).

To be precise about what tmux provides: **persistence, not supervision**. tmux keeps the Server alive across Client disconnects but will not restart it after a crash. The restart policy is therefore threefold:

1. The Server window runs a minimal respawn wrapper (restart on non-zero exit with backoff; break on clean exit), so a crashed Server comes back without human involvement.
2. `shevet connect` health-checks the Server and re-bootstraps it if dead — every connection is a self-heal point.
3. Hook Signals must not be lost while the Server is down: the hook command appends to a spool file (`~/.shevet/signals.spool`) whenever the unix socket is unreachable, and the Server drains the spool on startup. Status derived between crash and restart is delayed, not dropped.

Rejected: manual install + service manager (per-Host chore, user-owned version skew) and ephemeral per-connection servers (nobody listens for Hook Signals while disconnected, defeating always-on Status).

## Consequences

- The Client needs access to per-platform Server artifacts (release download or local cache) — it cannot copy itself across OS/arch boundaries.
- Client/Server protocol version skew self-heals on connect (upload newer binary, restart Server; tmux and Agents are untouched by a Server restart).
