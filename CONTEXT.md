# Shevet

A single-binary Go system that lets one developer monitor and drive AI execution agents (e.g. Claude Code) running inside tmux on machines they own, through a local terminal dashboard.

The name is Hebrew — שֵׁבֶט (*shevet*): the shepherd's rod/staff (Psalm 23:4), and simultaneously "tribe." The tool you herd with, and the group being herded.

## Language

**Herd**:
The full set of Agents a developer is monitoring, across all Hosts.

**Agent**:
An external AI execution process (e.g. Claude Code) running inside a tmux pane on a Host. Shevet observes and relays input to Agents; it does not implement them.
_Avoid_: bot, worker, task

**Status**:
The Server's judgment of what an Agent needs right now: **Running** (actively producing work), **Blocked** (halted, awaiting human input), **Idle** (finished its turn, nothing pending), **Exited** (process gone, Pane may remain). Status is per-Agent and flows to Clients the moment it changes.
_Avoid_: state (overloaded — reserve for terminal/grid state), health

**Signal**:
An input the Server uses to judge Status. Two kinds: a **Hook Signal** (structured event emitted by the Agent itself, e.g. Claude Code hooks — exact, carries a reason) and a **Screen Signal** (heuristic over the rendered Pane: output quiescence, prompt patterns near the cursor — approximate, works for any agent).

**Host**:
A machine owned by the developer that runs a Server daemon. One developer connects to one or more Hosts.
_Avoid_: node, cluster (implies multi-user infrastructure this system explicitly does not have)

**Server**:
The headless daemon role of the shevet binary. Runs on a Host, owns the tmux backend, and serves state to Clients.
_Avoid_: daemon-backend, engine

**Spawn**:
Creating a new Agent from the dashboard: the Server starts it inside the Shevet-owned tmux session on the chosen Host. Shevet owns a Spawned Agent's full lifecycle, including termination.

**Adopt**:
Bringing an existing tmux pane (started outside Shevet) into the Herd for observation, input, and Status. Shevet never terminates an Adopted Agent's process; removing it from the Herd just stops watching.
_Avoid_: attach (that's what a Client does to a Server), import

**Pane**:
The terminal surface of a single Agent, hosted in tmux on the Server's Host. The unit of viewing, input, and state detection.
_Avoid_: window, session (those are tmux grouping concepts, not the Agent surface)

**Client**:
The interactive TUI role of the shevet binary. Runs on the developer's local machine and renders state received from Servers.
_Avoid_: frontend, dashboard (dashboard is the *screen* the Client renders, not the process)

**Focus Lease**:
The exclusive right to send input and resize a Pane, held by at most one Client at a time. Entering a Pane takes the lease (stealing it from another Client if held); Clients without the lease view that Pane read-only.
_Avoid_: lock (a lease is stolen by focus, never waited on)

## Flagged ambiguities

- The spec says roles are chosen by "compilation flags" but also by startup flags. Resolution: **role by runtime invocation of one binary** (`shevet serve` vs `shevet connect`) — two builds would break the single-artifact deployment story.

## Example dialogue

> **Dev:** My Herd is three Agents — two on my homelab Host, one on a cloud Host.
> **Expert:** So you run one Server per Host, and your local Client attaches to both Servers at once?
> **Dev:** Right. The Client is just a viewer/controller; if my laptop dies, the Agents keep running because the Servers and tmux never noticed.
