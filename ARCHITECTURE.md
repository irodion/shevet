# Shevet Architecture

Detailed architecture for Shevet: a single-binary Go system that lets one developer monitor and drive AI execution agents (Claude Code and others) running inside tmux on machines they own, through a local terminal dashboard.

This document supersedes **"the spec"** — the original *Herdr Engine: Dual-Mode Unified Go System* design PDF (v1.0.0, written under the project's former name and since removed from the repo). Every deviation from it is recorded in `docs/adr/`. Where this document or an ADR cites the spec by section, the key is: §1 executive summary · §2 operational modes · §3 component stack (§3.1 Go runtime, §3.2 OpenTUI/Zig rendering, §3.3 headless tmux) · §4 network (mTLS, §4.1 the three streams) · §5 data flow (§5.1 16ms micro-buffer, §5.2 regex state detection) · §6 resiliency (§6.1 disconnect recovery) · §7 compilation and deployment.

Domain vocabulary lives in `CONTEXT.md`; capitalized terms here (Agent, Host, Pane, Status, Spawn, Adopt, Signal) are used in their glossary sense.

## 1. Scope

One developer. One Client (or several — laptop and tablet concurrently is supported; see the Focus Lease policy in §3.2) attached to Servers on N Hosts the developer owns. No multi-user identity, no tenancy, no shared infrastructure. Hosts must be unix (tmux is load-bearing; WSL counts as linux). The Client builds for any Go target, including Windows.

## 2. Topology

```
 Developer machine                        Host (one of N)
┌─────────────────────────┐   SSH        ┌──────────────────────────────────────┐
│ shevet connect <host>   │═════════════▶│ unix socket ~/.shevet/shevet.sock    │
│  Client (Bubble Tea v2) │  gRPC tunnel │  Server (shevet serve)               │
│  · dashboard chrome     │              │   · tmux -C control-mode attachment  │
│  · Pane grid widget     │              │   · per-Pane VT emulator + history   │
│  · SSH dialer/bootstrap │              │   · damage differ + coalescer        │
│  · OS notifications     │              │   · Status engine (Signals)          │
└─────────────────────────┘              │   · hook listener (same socket)      │
                                         │  tmux session "shevet"               │
                                         │   ├─ window: Server itself           │
                                         │   ├─ window: Spawned Agent A         │
                                         │   └─ window: Spawned Agent B         │
                                         │  other tmux sessions                 │
                                         │   └─ pane: Adopted Agent C           │
                                         └──────────────────────────────────────┘
```

One binary, roles selected at runtime (ADR: runtime flags, never separate builds). CLI shape: subcommands rather than a literal `--mode` flag — `shevet serve` (Server), `shevet connect <host>` (Client), `shevet host enable-boot`, etc. — honoring the spec's intent (single artifact, role by invocation) with a conventional CLI surface.

## 3. Component stack

### 3.1 Server

- **Process control: headless tmux (`tmux -C`).** The Server attaches one control-mode client to the tmux server and consumes `%output`, `%pane-*`, `%window-*`, `%exit` notifications. tmux owns PTYs and process persistence; the Server owns interpretation. Kept from the spec unchanged — this is the same mechanism iTerm2's tmux integration uses.
- **Terminal state: per-Pane pure-Go VT emulator** (ADR-0001). `%output` bytes feed the emulator; it maintains the authoritative cell grid, cursor, and a bounded scrollback (default 10k lines/Pane). Library: **`charmbracelet/x/vt`**, pinned by commit, wrapped behind an internal `Emulator` interface (ADR-0006; bench results in docs/research/vt-emulator-selection.md — it was the only living candidate with correct wide-char/ZWJ grid parity, typed damage tracking, and built-in scrollback).
- **Damage pipeline** (refined from spec §5.1): the emulator marks dirty cells; a per-Pane coalescer flushes at most every 16ms — *event-driven one-shot timer, not a free-running ticker* — so idle Panes cost zero wakeups. Damage is coalesced per cell (last write wins), making updates idempotent and bounded regardless of Client speed; a Client that falls too far behind receives a full-grid snapshot instead of a queue.
- **Status engine** (ADR-0004): layered Signals per Agent.
  1. **Hook Signals** (primary, exact): Shevet installs/documents Claude Code hook configs (`Notification`, `Stop`) whose command pings the Server's unix socket with `{pane, event, reason}`. If the socket is unreachable (Server down), the hook command appends the event to `~/.shevet/signals.spool`; the Server drains the spool on startup, so Signals emitted during Server downtime are delayed rather than lost (ADR-0005).
     **Spool protocol** (the guarantee is only as strong as this contract):
     - *Framing:* JSONL — one record per line: `{id, pane, event, reason, ts}`. `id` is a ULID minted by the hook command; `ts` is the hook's wall clock.
     - *Concurrent appends:* file opened `O_APPEND|O_CREAT`, one `write(2)` per record, records well under 4 KB — appends from concurrent hooks don't interleave. No fsync: the loss window is a host crash, which kills the Agents themselves anyway.
     - *Drain:* on startup the Server atomically `rename(2)`s `signals.spool` → `signals.spool.draining` (hooks recreate a fresh spool on next append), replays it, then deletes it. A pre-existing `.draining` file (crash mid-drain) is replayed *before* the current spool.
     - *Replay order:* strictly **append order** (`.draining` file first, then the spool, each top-to-bottom). Append order is causal order per Pane — a hook appends when its event fires, and one Agent's hooks are sequential — whereas wall-clock `ts` can tie or step across hook processes and ULIDs minted independently carry no causal sequence. `ts` is diagnostic only; cross-Pane ordering is irrelevant (Status is per-Agent).
     - *Stragglers:* a hook can lose the race with a restarting Server (socket probe fails → it spools *after* the startup drain). The Server therefore watches the spool path after startup (inotify/kqueue, with a slow poll as fallback) and drains late records the same way — a spooled Signal never waits for the *next* restart.
     - *Idempotent replay:* crash-mid-drain means records can be applied twice, so replay dedupes by `id` (the Server persists the set of applied ids alongside the draining file) and Status transitions are idempotent — re-applying an event to an Agent already in that Status is a no-op.
  2. **Screen Signals** (fallback, generic): output-quiescence timer + prompt patterns evaluated against *rendered rows near the cursor* — never against raw bytes. Per-agent adapter profiles declare which patterns apply.
  - Status enum: `Running`, `Blocked(reason)`, `Idle`, `Exited`. Transitions broadcast on the Telemetry Stream.
- **Listener:** gRPC on `~/.shevet/shevet.sock` (mode 0600). No TCP listener by default (ADR-0003).

### 3.2 Client

- **TUI: Bubble Tea v2 + bubbles + lipgloss** (ADR-0002). Cell-level diffing is native to the v2 renderer; the visual bar is Crush-grade theming (truecolor, tinted rows, RGB-blended modal overlays computed over the Client-held grid — terminals have no real alpha; blending is arithmetic we own).
- **Pane widget (custom):** holds the local grid per viewed Pane, applies damage patches, renders into the frame. Scrolling issues paged history requests (`rows -200..-150`) served from Server-side scrollback.
- **Input model: modal + leader key.** Dashboard mode drives the UI; entering a Pane switches to passthrough where every keystroke is re-encoded to exact bytes and forwarded — only the configurable leader (default `Ctrl-\`) is reserved.
- **Injection on the Host, precisely** (tmux's tools each have narrow semantics; batched input is split into runs by type):
  - *Printable text (incl. UTF-8):* `send-keys -l -t <pane> --` — literal mode, no key-name lookup, tmux handles multi-byte text natively. (`send-keys -H` is **not** used for bytes ≥ 0x80: tmux interprets such values as Unicode codepoints, which would double-encode raw UTF-8.)
  - *Control bytes and escape sequences* (arrow keys, chords — always 0x00–0x7F): `send-keys -H` per byte, where hex is unambiguous.
  - *Paste:* `load-buffer` + `paste-buffer -p` — tmux brackets iff the pane requested bracketed paste; that mode stays delegated to tmux.
  - *Mouse:* `send-keys -M` cannot synthesize events (it only forwards a real event from a tmux binding), so the Server **encodes mouse reports itself**: it reads the pane's live mouse-mode formats (`mouse_any_flag`, `mouse_standard_flag`, `mouse_sgr_flag`, `mouse_utf8_flag`, …), emits the matching sequence (SGR `\e[<b;x;yM`, X10, urxvt) as plain ≤0x7F bytes via the paths above — or, when no mouse mode is active, interprets the event Client-side (e.g. wheel = scrollback paging) instead of injecting. Reading the formats at injection time means there is no mouse state to restore after a Server restart.
- **Focus Lease (multi-Client policy).** A tmux pane has one real size and one input stream, so concurrency is resolved by lease, not by merging: at most one Client holds a Pane's Focus Lease; entering a Pane acquires it, stealing from any current holder (it's the same developer — steal, never block). Only the lease holder's input and `ResizeRequest` are honored; the Pane's canonical size follows the holder, and non-holders render the canonical grid read-only (scrolling their viewport if smaller). Lease changes broadcast on the Telemetry Stream so every Client can show who's driving.
- **Alerting:** on `Blocked`/`Exited` transitions the attached Client raises an OS notification (OSC 9 / bell / notifier command) in addition to the badge. Detached alerting (webhook/push from the Server) is deferred; the Telemetry Stream already carries the events, so a webhook sink is additive later.
- **Multi-host:** the Client dials several Hosts concurrently; the Herd view aggregates Agents across connections.

## 4. Protocol

gRPC + Protobuf (kept from spec) over the SSH-tunneled unix socket (ADR-0003). Three long-lived streams, per spec §4.1, with contents sharpened:

| Stream | Direction | Payload |
|---|---|---|
| Matrix Render | Server → Client | `CellDamage{pane, cells[]{x,y,ch,fg,bg,attrs}}`, `GridSnapshot`, `CursorMove`, `PaneResized` |
| Control Input | Client → Server | `AcquireFocus{pane}`, `KeyBytes{pane, bytes}`, `Paste{pane, bytes}`, `Mouse{pane, x,y,btn,mods}`, `ResizeRequest` — input/resize honored only from the Focus Lease holder |
| Telemetry | Server → Client | `StatusChanged{agent, status, reason, at}`, `AgentAdded/Removed`, `FocusChanged{pane, holder}`, `HostHealth` |

Plus unary RPCs: `ListPanes`, `Spawn{host_dir, command}`, `Adopt{tmux_pane_id}`, `Release`, `Kill` (Spawned Agents only — Adopt semantics forbid killing, see CONTEXT.md), `FetchHistory{pane, from, to}`, `ServerInfo` (version handshake).

Versioning: protobuf fields are additive; `ServerInfo` gates incompatible changes and triggers re-bootstrap (§6).

## 5. Lifecycles

### 5.1 Agent lifecycle
- **Spawn:** Client picks Host, working dir, command → Server creates a window in the `shevet` tmux session. Cradle-to-grave ownership, `Kill` allowed.
- **Adopt:** Server enumerates non-Shevet tmux panes on the Host; the developer adopts one into the Herd. Shevet observes, injects input, and derives Status, but never terminates it; `Release` just stops watching.

### 5.2 Disconnection (network loss — spec §6.1, kept with amendments)
1. Server drops the dead gRPC stream; tmux and Agents are untouched; Status engine keeps running (Hook Signals keep arriving).
2. Client freezes the last frame under a reconnect overlay and redials SSH with backoff.
3. On reattach the Server sends `GridSnapshot` per viewed Pane **from emulator memory** — `capture-pane` is *not* on this path (ADR-0001) — plus a Telemetry state-of-the-world message. Missed `Blocked` transitions surface as notifications on reattach.

### 5.3 Server restart (gap in the spec, now covered)
tmux outlives the Server. Restart has two layers: the respawn wrapper in the Server's tmux window restarts a crashed Server automatically, and `shevet connect` re-bootstraps a dead one on every connection (ADR-0005).

Recovery is a **degraded reconstruction**, stated honestly:
- Visible content + SGR styling reseed from `capture-pane -e` (and `-a` for the alternate screen where present).
- **Restored modes — the explicit acceptance set**, read from tmux's per-pane format variables (tmux tracks these authoritatively even though `capture-pane` doesn't emit them): `alternate_on` (+ `alternate_saved_x/y`), `cursor_x`/`cursor_y`, `cursor_flag` (visibility), `insert_flag` (IRM), `keypad_cursor_flag` (DECCKM application cursor keys), `keypad_flag` (application keypad), `wrap_flag` (DECAWM), `origin_flag` (DECOM).
- **Modes Shevet deliberately does not restore** because they are read live at injection time, from state tmux never lost (§3.2): bracketed paste (tmux applies it inside `paste-buffer -p`) and all mouse modes (the Server consults the pane's `mouse_*_flag` formats on every mouse event).
- **Accepted, documented loss:** extended-key/kitty-keyboard negotiation state between the inner application and tmux is not reconstructed into the emulator's view; since keys are injected as raw bytes (`send-keys -H`), the practical effect is limited to rare mis-encoded modifier chords in apps using the kitty protocol, until the app renegotiates.
- Genuinely lost: in-memory scrollback accumulated before the restart, and any mid-escape-sequence parser state (sub-frame, negligible).
- Hook Signals emitted during the outage replay from the spool file (per the spool protocol in §3.1, duplicates are deduped by id); Status re-derives from replayed Signals plus the reseeded screen.

### 5.4 Host reboot
Opt-in `shevet host enable-boot` installs a systemd user unit / launchd plist that starts tmux + Server at boot. Agents do not survive a reboot (they are OS processes); the Herd view reports them `Exited`.

## 6. Bootstrap & deployment (ADR-0005)

- `shevet connect <host>`: SSH in → `ServerInfo` probe → if binary missing/stale, upload the correct GOOS/GOARCH artifact (from release download or local artifact cache — the Client cannot copy itself across platforms) → start Server *inside the `shevet` tmux session* under the respawn wrapper (tmux provides persistence; the wrapper provides crash restart — ADR-0005) → tunnel gRPC.
- **Tunnel mechanism, concretely:** primary path is the OpenSSH `direct-streamlocal@openssh.com` channel — `golang.org/x/crypto/ssh`'s `Client.Dial("unix", path)` — which forwards straight to the Server's unix socket with no remote helper. Fallback for SSH servers without streamlocal support: `ssh exec` of `shevet _proxy`, a hidden subcommand of the already-present binary that pipes stdio ↔ unix socket. Either way the Host dependency surface stays `tmux` only — no `socat`/`nc` (ADR-0003).
- Build: `CGO_ENABLED=0` throughout (no Zig, no CGO — ADR-0002); cross-compilation is a plain GOOS/GOARCH matrix. Host dependency surface: `tmux` only.
- Upgrade: ship a new binary; connect re-bootstraps Servers. Server restarts are non-disruptive to Agents (§5.3).

## 7. Deliberate exclusions

- No web UI, no browser runtime (premise of the whole design).
- No mTLS/PKI in v1 (ADR-0003); no open TCP ports on Hosts.
- No multi-user auth/tenancy (scope).
- No Windows Hosts (tmux); Windows Client is fine.
- No server-side push/webhooks in v1 (additive later via Telemetry sink).
- No Pane image rendering (sixel/kitty graphics) in v1 — the emulator consumes and discards image sequences. The investigated two-tier path to add it later without breaking anything is preserved in ADR-0007.
- No regex over raw byte streams, ever (ADR-0004).

## 8. Open implementation questions (not architectural forks)

- ~~VT emulator library selection spike~~ — resolved: `charmbracelet/x/vt` (ADR-0006, docs/research/vt-emulator-selection.md).
- Hook installer UX: write into the Host project's `.claude/settings.json` vs user-level config; how to namespace and cleanly uninstall.
- Artifact distribution channel for bootstrap (GitHub releases URL vs `~/.shevet/artifacts` cache) and checksum verification.
- Mouse encoding matrix: the design is settled (§3.2 — Server encodes reports per the pane's `mouse_*_flag` formats); remaining work is the full encoder table (X10 vs UTF-8 vs SGR vs urxvt coordinate encodings and their limits, e.g. X10's 223-column cap).
- Screen Signal profile format (per-agent quiescence thresholds + prompt patterns) and where profiles live.
