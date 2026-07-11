// Package inject turns a Client's raw input bytes into tmux send-keys
// commands and delivers them to a Pane, byte-for-byte.
//
// tmux offers two ways to synthesize input, and picking the wrong one for a
// byte corrupts it — so the encoding rules here are load-bearing, not a
// convenience (ARCHITECTURE.md §3.2, "Injection on the Host, precisely"):
//
//   - Printable text, including multi-byte UTF-8, goes through
//     `send-keys -l -- <text>`: literal mode, no key-name lookup, tmux emits
//     the bytes as typed. Crucially this is the *only* path for bytes ≥ 0x80.
//     `send-keys -H` interprets its hex as a Unicode codepoint, so feeding it
//     a raw UTF-8 byte like 0xD1 would re-encode it and double the bytes.
//   - Control bytes (C0, 0x00–0x1F, and DEL, 0x7F) go through `send-keys -H`
//     per byte, where the hex value is unambiguous — an Enter, a Ctrl-C, the
//     ESC that opens an arrow-key sequence. They must never ride the literal
//     path, where tmux would apply key-name lookup or output processing.
//
// The stream is split into maximal runs of one class each, preserving order,
// so a mixed sequence like ESC "[" "A" (cursor up) or "café\r" arrives at the
// Pane's pty exactly as the Client encoded it. The classification is purely
// by byte value, which makes it total and testable independent of tmux.
package inject

import (
	"context"
	"fmt"
	"strconv"
)

// Commander runs one tmux command and returns its reply. *tmuxctl.Client
// satisfies it; tests substitute a recorder. Command must quote its
// arguments for tmux's parser, as tmuxctl does, so literal runs with spaces
// or metacharacters pass through unaltered.
type Commander interface {
	Command(ctx context.Context, args ...string) ([]string, error)
}

// Keys injects data into the tmux pane paneID, splitting it into send-keys
// runs by byte class and issuing them in order. It returns on the first
// command that fails, so a partial injection surfaces rather than hides;
// empty data is a no-op. paneID is any tmux target the Herd exposes (a %N
// pane id).
func Keys(ctx context.Context, ctl Commander, paneID string, data []byte) error {
	for i := 0; i < len(data); {
		if isControl(data[i]) {
			j := i + 1
			for j < len(data) && isControl(data[j]) {
				j++
			}
			if err := sendControl(ctx, ctl, paneID, data[i:j]); err != nil {
				return err
			}
			i = j
			continue
		}
		j := i + 1
		for j < len(data) && !isControl(data[j]) {
			j++
		}
		if err := sendLiteral(ctx, ctl, paneID, data[i:j]); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// Resize resizes the tmux window holding paneID to w×h cells, realizing a
// Client's ResizeRequest (resize-on-focus, ARCHITECTURE.md §3.2). tmux sizes
// windows, not panes, and this leans on the one-Pane-per-window shape Spawn
// guarantees: resizing the window is resizing the Pane. For a Pane sharing
// its window (a hand-made split today, Adopted Panes later), the window
// resize lands but the Pane gets only its share — degraded, not corrupt;
// the Adopt slice must revisit this seam. tmux switches the window to
// manual sizing, reflows, and reports the new geometry through its usual
// notifications — the watcher's reconcile picks the change up from there,
// so confirmation rides the render stream (PaneResized), never this call.
func Resize(ctx context.Context, ctl Commander, paneID string, w, h int) error {
	if _, err := ctl.Command(ctx, "resize-window", "-t", paneID,
		"-x", strconv.Itoa(w), "-y", strconv.Itoa(h)); err != nil {
		return fmt.Errorf("resize pane %s to %dx%d: %w", paneID, w, h, err)
	}
	return nil
}

// isControl reports whether b must be injected by hex rather than literally:
// the C0 control bytes (0x00–0x1F) and DEL (0x7F). Every other byte —
// printable ASCII and all UTF-8 continuation and lead bytes (≥ 0x80) — is
// literal text. This split is the whole correctness argument of the package.
func isControl(b byte) bool {
	return b < 0x20 || b == 0x7f
}

// sendLiteral delivers a run of printable/UTF-8 bytes as one literal
// send-keys. `--` terminates option parsing so a run beginning with '-' is
// still treated as text.
func sendLiteral(ctx context.Context, ctl Commander, paneID string, run []byte) error {
	if _, err := ctl.Command(ctx, "send-keys", "-t", paneID, "-l", "--", string(run)); err != nil {
		return fmt.Errorf("inject literal into pane %s: %w", paneID, err)
	}
	return nil
}

// sendControl delivers a run of control bytes as one hexadecimal send-keys —
// `send-keys -H 1b 5b ...` — so the whole run is one tmux round-trip. Hex
// values can never be mistaken for options, so no `--` is needed.
func sendControl(ctx context.Context, ctl Commander, paneID string, run []byte) error {
	args := make([]string, 0, 4+len(run))
	args = append(args, "send-keys", "-t", paneID, "-H")
	for _, b := range run {
		args = append(args, fmt.Sprintf("%02x", b))
	}
	if _, err := ctl.Command(ctx, args...); err != nil {
		return fmt.Errorf("inject control bytes into pane %s: %w", paneID, err)
	}
	return nil
}
