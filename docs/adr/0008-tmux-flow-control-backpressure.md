# Output ingest backpressure is tmux's flow control, not a Server-side queue; the tmux floor is 3.2

The path from tmux to the emulator had no backpressure: tmuxctl's event queue is deliberately unbounded (the control-stream reader must never block, or a consumer waiting on a command reply deadlocks), and a Pane can emit faster than the emulator consumes. A sustained flood therefore accumulated output on the Server heap without bound — #48 reproduces multi-GB RSS in about a minute, with no Client attached at all.

The decision: the Server attaches every control-mode client with tmux's pause flow control (`pause-after`, tmux ≥ 3.2). When a Pane's output outruns the Server, **tmux pauses that Pane's stream** (`%pause`) instead of the Server buffering it; once the backlog drains, the Server resumes the Pane (`refresh-client -A`) and re-seeds it through the existing capture-pane + seed-barrier path.

How the pause is triggered matters: `pause-after` ages output sitting in *tmux's* write queue, and a Server that reads the control stream eagerly (it must — command replies arrive on the same stream) keeps that queue empty. So the primary trigger is the Server itself: when a Pane's pipeline is saturated, the Server explicitly pauses that Pane (`refresh-client -A '%id:pause'`) and discards its already-queued output — the resume re-seed supersedes it. `pause-after` remains as the backstop for whole-stream overload, where reads genuinely slow down. Either way, memory for a flooding Pane is bounded by the pipeline's queue, not by the flood. With flow control on, pane output arrives as `%extended-output` (carrying an age field), which the notification parser must handle alongside `%output`. A paused-then-resumed Pane is exactly the degraded reconstruction ARCHITECTURE §5.3 already defines — no new recovery machinery, and the unbounded queue's original assumption (it only grows during single command round-trips) becomes true again.

Two ingest costs go away regardless of flow control:

- The emulator's own scrollback is **disabled**. Shevet never reads it (Server-side history is its own store — the scrollback slice), and at the 10k-line cap it taxed every scrolled line with an O(10k) eviction.
- Queued output ops are **coalesced into one emulator write per pipeline wakeup**, so parser overhead amortizes across a burst.

This sets the minimum supported tmux to **3.2** (released 2021), where control-mode pause shipped.

Rejected: self-managed dropping (bound the queue, drop a flooding Pane's output, mark it for re-seed) — it reimplements what tmux already does, and dropping without tmux's cooperation can tear mid-escape-sequence; keeping the queue unbounded (the status quo #48 measured).

## Consequences

- `shevet serve` should verify the tmux version at attach and fail with a clear message below 3.2.
- The Server must handle `%pause`/`%continue` notifications; a paused Pane is a normal, visible state (it will typically resume within one flush interval).
