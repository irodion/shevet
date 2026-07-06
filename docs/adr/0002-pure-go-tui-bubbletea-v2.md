# Client TUI is pure Go (Bubble Tea v2 + lipgloss), not OpenTUI's Zig core via CGO

The spec called for binding OpenTUI's Zig core into Go via CGO for a "modern-looking" low-latency TUI. We rejected that: OpenTUI's modern look lives in its **TypeScript layer** (components, flexbox, styling) — the Zig core is only a cell framebuffer, so binding it would still leave us building the entire component system in Go while paying for CGO, hand-maintained C-ABI bindings, and a permanent `zig cc` cross-compilation toolchain. Since ADR-0001 moved diffing server-side, the client's render job is light enough that the Zig core solves a problem this architecture no longer has.

We use **Bubble Tea v2** (whose renderer diffs at cell level), `bubbles` components for dashboard chrome, and `lipgloss` for theming — the same stack as Charm's Crush, which demonstrates the visual ceiling we're aiming for. Pane content is a custom widget that applies server cell-damage patches into a local grid. This keeps `CGO_ENABLED=0` and single-command static cross-compilation.

## Consequences

- Overlay/modal translucency is simulated by our own RGB blending over the client-held grid (terminals have no real alpha; OpenTUI simulates it the same way).
- Bubble Tea v2 API churn is an accepted risk; versions are pinned.
- If OpenTUI ever ships supported Go bindings including its component layer, revisit.
