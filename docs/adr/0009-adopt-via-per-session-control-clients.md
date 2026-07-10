# Adopt observes foreign sessions through additional per-session control clients

A tmux control-mode client only receives `%output` (and topology notifications) for panes in windows linked to the session it is attached to. The Server's single client is attached to the `shevet` session, so a Pane Adopted from another session would be silent — Adopt as designed (ARCHITECTURE §5.1) had no observation mechanism.

The decision: the watcher manages **one control-mode client per tmux session that contains Adopted Panes** — attached on the first Adopt into a session, detached on the last Release from it. Each client reuses the same machinery the primary one already has (notification parsing, capture-pane seeding with the sequence barrier, reconcile), scoped to its session. Adoption never mutates the developer's tmux topology: Shevet joins the session as one more (control) client, exactly what Adopt's observe-don't-own semantics promise.

Rejected:

- **`link-window` into the `shevet` session** so the existing single client sees the adopted window. One client and no new machinery, but it visibly mutates the developer's topology, forces window-granular adoption (a pane's siblings come along), and a window linked into two sessions is exposed to cross-session window-sizing rules.
- **`pipe-pane -O` per adopted pane.** Output only: no resize or topology notifications, no ordering seam with capture-pane seeds (the barrier that makes seed + live stream compose is control-stream sequence numbers, which a pipe doesn't have), and it collides with any `pipe-pane` the developer runs themselves.

## Consequences

- The watcher grows a client-manager: reconcile spans several control clients, and a foreign session ending is a per-client stream end, not a Server-wide failure.
- Each adopted-from session costs one extra `tmux -C` client process on the Host — negligible at one-developer scale.
