---
status: proposed — optional follow-up, not scheduled; recorded to preserve the investigation
---

# Pane image support (sixel / kitty graphics), if ever needed, is built in two tiers

Shevet v1 deliberately renders text only: the Server emulator consumes and discards image escape sequences (APC/DCS), and nothing in the protocol carries pixels. Agents like Claude Code don't emit images, so this costs nothing today. This ADR records *how* image support would be added, because the investigation found the pipeline has five hops with non-obvious failure points, and the findings are worth keeping.

## The five hops an image must survive

1. **Agent emits** — only after probing terminal capability. Inside a Pane the probe reaches whoever answers on the PTY, so the Server emulator must answer DA1/DA2 and kitty-graphics queries claiming support (x/vt's `InputPipe()` carries the replies; inject back via `send-keys`). Without this lie, nothing downstream ever happens.
2. **tmux relays** — the shakiest hop. Sixel requires tmux built with `--enable-sixel`, and what control mode's `%output` relays of a parsed sixel must be verified empirically. The kitty protocol is worse: tmux doesn't model it; its `allow-passthrough` wrapper targets a real attached terminal, which a control-mode client is not.
3. **Server models** — x/vt hooks exist (`RegisterDcsHandler` for sixel `q`, `RegisterApcHandler` for kitty `_G`): decode, store image objects `{id, cell rect, z-order}`, and treat placement/scroll/eviction as damage over the covered rectangle.
4. **Wire carries** — new protocol messages `ImagePlaced{pane, image_id, cell_rect, format}` plus content-addressed blob transfer (hash-dedupe, fetch-on-miss, Client cache). Never inline pixels in the damage stream.
5. **Client re-emits** — translate to the local terminal's dialect (kitty / sixel / iTerm2 OSC 1337). The hard engineering: compositing floating images over a cell-based Bubble Tea UI (absolute positioning, z-order vs. chrome, cleanup on scroll/view-switch) — the part that makes notcurses/yazi nontrivial.

## The tiers

- **Tier 0 — rasterize server-side.** Decode images on the Server and downscale to half-block cell mosaics (`▀` + per-cell fg/bg RGB). Output is ordinary cells: zero protocol changes, zero Client changes, works in every terminal. Fidelity is "pixelated thumbnail" — sufficient to see *what* an agent displayed. Touches hops 1–3 only. This is the first increment if images ever matter.
- **Tier 1 — native fidelity.** Full hops 4–5: image objects in the protocol, native re-emission on capable terminals, cell-mosaic fallback elsewhere. Only worth it if agents start producing images users must actually read (charts, screenshots for review).

## Prior art (researched 2026-07-06)

How existing multiplexers solve this confirms the tier design and adds hard-won lessons:

- **Zellij** (sixel since 0.31.0, via their `sixel-image`/`sixel-tokenizer` crates): interprets sixel **server-side** into a global deduplicated image store + per-pane placement rectangles, and re-serializes viewport-intersecting crops to the client — structurally our Tier 1. Kitty protocol: unsupported, no passthrough mechanism at all ([#2814](https://github.com/zellij-org/zellij/issues/2814), [#4500](https://github.com/zellij-org/zellij/issues/4500)). Cautionary warts: its DA1 reply advertises sixel *unconditionally*, breaking capability detection for inner apps when the real terminal can't render ([#3158](https://github.com/zellij-org/zellij/issues/3158)); the sixel path has known perf problems ([#3981](https://github.com/zellij-org/zellij/issues/3981)); and its no-support fallback is a blank gap.
- **tmux** (sixel since 3.4, `--enable-sixel`): same interpret-and-re-emit model (`image.c`); draws a `~` placeholder box on non-sixel terminals; only advertises sixel in DA1 when the outer terminal has it (the honest behavior zellij lacks). Kitty protocol: blind passthrough only (`allow-passthrough`) — tmux has no model of those images, so they don't survive redraw/scroll/reattach.
- **WezTerm mux**: the cleanest reference — accepts all three protocols, normalizes at parse time into hash-deduplicated `ImageData` blobs referenced by *cell attachments*, and its mux protocol ships structured placements, not escape streams. First-class cross-protocol support, but even they hit image-over-mux perf issues ([#1237](https://github.com/wezterm/wezterm/issues/1237)).
- **iTerm2 tmux control mode**: images cross `%output` only via tricks — `allow-passthrough` or iTerm2 3.5's *multipart* OSC 1337 variant designed specifically to survive control mode; persistence is entirely client-side because tmux doesn't model them.

Lessons folded into this design: (1) interpret-and-re-emit is the only architecture where images survive scroll/reattach — passthrough is a dead end for a server-side-grid system; (2) separate image bytes (content-addressed store) from placements, and cache serialized forms — per-frame re-encoding is the documented perf killer in both zellij and wezterm; (3) capability answering must be *honest per attached Client* (zellij's unconditional advertise is the anti-pattern); (4) the half-block cell fallback (Tier 0) is better UX than both zellij's blank gap and tmux's `~` box — `sixel-tmux` proved this approach; (5) sixel is the tractable protocol to interpret; kitty's (chunked uploads, IDs, z-index, mandatory query responses) is a far bigger state machine — support sixel first, kitty later or never.

## Why deferred

No current Agent emits images; hop 2 may require a specific tmux build; and Tier 1's Client compositing is a large, framework-fighting effort. Nothing in v1's design blocks either tier: the emulator sits behind our own interface (ADR-0006), damage is rectangle-based, and the protocol is additive (ADR-0003 kept gRPC/Protobuf). Revisit when a real Agent use case appears — starting with Tier 0 and an empirical test of hop 2.
