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
Exclusive authority over a Pane's geometry — its canonical size and the eventual restore of its pre-drive size — held by at most one Client at a time. A Pane resize (a Focus act) steals the lease from any holder; keystrokes, selection, viewing, and tile rearrangement never do — input is always delivered without moving geometry authority, so typing into a tile cannot strand another Client's Focus. Only the holder's restore, or its disconnect, returns the Pane to its pre-drive size.
_Avoid_: lock (a lease is stolen by resize, never waited on)

**Tiles**:
The dashboard's default surface: the whole Client viewport divided among the Herd, one live tile per Pane while readable room remains — a Pane joining splits a tile, a Pane leaving returns its space, and Panes past readable capacity roll up into a single "+N more" tile. The selected tile is live: plain keystrokes flow to its Agent; every dashboard verb sits behind the one reserved leader. The developer organizes tiles (order, size); the layout never centers, pads, or leaves the viewport unused.
_Avoid_: grid (reserved for the cell-grid vocabulary of a Pane's screen), mosaic

**Overview**:
A transient orientation surface — the OS window-overview analogue: the Herd as uniform cards for triage at a glance. Opened by a dedicated shortcut; picking a Pane jumps straight to Focus on it, dismissing returns to Tiles. Never dwelled in, never receives Agent-bound keystrokes.
_Avoid_: mosaic, card view, grid view

**Focus**:
The surface that devotes the dashboard to a single Pane for direct interaction; entering resizes the Pane to the viewport — a driving act, so the Focus Lease follows. A Shevet bar stays visible — Focus narrows attention to one Agent without blinding the developer to the Herd.
_Avoid_: zoom, fullscreen (a bar remains), maximize

**Leader**:
The single reserved key that opens the dashboard's verb layer; every other keystroke in Tiles and Focus belongs to the Agent. Configurable — the one key Shevet withholds from Agents.
_Avoid_: prefix (tmux's word — inviting confusion with the tmux server underneath), hotkey

## Flagged ambiguities

- The spec says roles are chosen by "compilation flags" but also by startup flags. Resolution: **role by runtime invocation of one binary** (`shevet serve` vs `shevet connect`) — two builds would break the single-artifact deployment story.

## Example dialogue

> **Dev:** My Herd is three Agents — two on my homelab Host, one on a cloud Host.
> **Expert:** So you run one Server per Host, and your local Client attaches to both Servers at once?
> **Dev:** Right. The Client is just a viewer/controller; if my laptop dies, the Agents keep running because the Servers and tmux never noticed.
