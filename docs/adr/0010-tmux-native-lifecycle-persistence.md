# tmux user options and remain-on-exit are the durable store for Pane identity and lifecycle

A Server restart must not lose three things tmux does not obviously record: which pane is the Server's own window (it must never be registered as an Agent), each Agent's provenance (Spawned vs Adopted — a hard ownership boundary: `Kill` is legal only against Spawned), and Exited Spawned Panes that must retain their final screen until explicitly Dismissed.

tmux outlives the Server (that is the whole §5.3 restart story), so tmux holds all three — no manifest file, no second source of truth that can drift:

- **Server identity:** bootstrap tags the Server's own window with a user option (`@shevet_role=server`) when it creates it; reconciliation reads the option in its `list-panes` format and excludes tagged panes from the Herd.
- **Provenance:** Spawn and Adopt set `@shevet_provenance=spawned|adopted` on the pane; Release clears it. A restarted Server reads provenance back from the pane itself.
- **Exited-until-Dismissed:** the `shevet` session runs `remain-on-exit on`. A Spawned Agent's death leaves a dead pane in tmux with its final screen intact — `capture-pane` still works, Status `Exited` derives from `#{pane_dead}` — until Dismiss, which is `kill-pane`. The retained screen survives Server restarts because it never left tmux.

Retention is scoped to Spawned Agents. Adopted Panes follow their own session's `remain-on-exit` configuration: setting it would mutate a session Shevet doesn't own, which Adopt semantics forbid. A user option on the adopted pane itself (set on Adopt, removed on Release) is the one deliberate, invisible exception.

Rejected: a durable manifest/journal under `~/.shevet` (a parallel record of what tmux already knows, guaranteed to drift from it across crashes); weakening the guarantees (documenting that restart forgets provenance and retained screens — #15's acceptance criteria would have to be softened for no saving elsewhere).

## Consequences

- Everything above dies with the tmux server. That is already Shevet's stance: tmux death kills the Agents themselves; there is nothing meaningful to remember past it.
- Reconcile's `list-panes` format grows the user-option fields; registry entries carry provenance from birth.
