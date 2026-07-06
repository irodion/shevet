# Server-side VT emulator is charmbracelet/x/vt, pinned by commit, behind our own Emulator interface

We benched five candidates against the ADR-0001 acceptance bar (see docs/research/vt-emulator-selection.md). The deciding requirement is **tmux grid parity on wide characters**: the Server's grid must agree column-for-column with tmux or input coordinates, cursor position, and Screen Signals drift on any CJK/emoji output. Only charm x/vt passed that (spacer cells, ZWJ clusters as one cell) while also being alive, having typed damage tracking (`Touched()`, `CellDamage`/`ScrollDamage`) and built-in bounded scrollback — a one-to-one fit for our damage-streaming pipeline.

Why this is surprising without context: we chose an **untagged, explicitly experimental module** over the semver-tagged, Dagger-backed midterm. midterm (and vt10x) have no wide-char handling at all — fixing that ourselves was judged worse than pinning x/vt to an exact commit and wrapping it behind a small internal `Emulator` interface (`Write / CellAt / Cursor / Damage / Resize / Scrollback`) that keeps it swappable. midterm is the designated fallback.

## Consequences

- Pin an exact x/vt commit; upgrades are deliberate, reviewed events (its damage/scrollback APIs are months old and the repo promises no compatibility).
- One goroutine owns each Emulator instance end-to-end, which also sidesteps x/vt's open `Close()` race (charmbracelet/x#879).
- Parser throughput is ~6.8 MB/s per pane (5× slower than st-lineage) — ~100× above realistic agent output; revisit only if profiling ever says otherwise.
- Known landmine to avoid: never add `vito/midterm` and `danielgatis/go-headless-term` to the same module (conflicting `go-ansicode` interface versions).
