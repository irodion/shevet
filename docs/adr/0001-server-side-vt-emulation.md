# Server owns the terminal state: VT emulation runs server-side, cell damage goes over the wire

The original spec contradicted itself: it said the Server never parses ANSI (§3.3) yet streams cell-grid damage to the Client (§4.1). `tmux -C` emits raw escaped bytes, so someone must run a VT emulator. We decided the **Server** feeds tmux `%output` into a per-pane pure-Go VT emulator, owns the authoritative cell grid, and streams cell damage; the Client is a dumb grid renderer.

## Considered Options

- **Client-side emulation (raw byte relay)** — rejected: reconnect needs a `capture-pane` replay dance, agent-state detection has to regex raw bytes full of escape sequences, and every attached client re-parses everything.
- **tmux-as-emulator (poll `capture-pane -e`)** — rejected as primary path: turns an event-driven design into a polling loop and loses cursor/scroll semantics. Acceptable as a resync fallback only.

## Consequences

- Agent-state detection reads the *rendered* screen (cursor position + visible rows), which is far more robust than pattern-matching a raw stream.
- Reconnect is trivial: the Server resends the full grid from memory; `tmux capture-pane` is not on the critical path.
- Panes not currently viewed cost the Client nothing; multiple Clients are cheap.
- The Server carries a VT emulator dependency and its edge cases (wide chars, scroll regions, alt screen).
- Scrollback lives in the Server-side emulator (bounded N lines per Pane); Clients page history over the protocol instead of shelling out to `tmux capture-pane` per scroll.
