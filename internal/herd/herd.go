// Package herd holds the Server's authoritative registry of Panes.
//
// The Registry is the single source of truth for which Panes exist in the
// Herd (see CONTEXT.md). In this slice it is an empty, concurrency-safe
// shell; later slices populate it from tmux control-mode events and add the
// Spawn/Adopt lifecycle.
package herd

import "sync"

// Pane is one Agent's terminal surface managed by the Server.
type Pane struct {
	// ID is the Server-scoped stable identifier of the Pane.
	ID string

	// Title is a human-readable label for dashboards.
	Title string
}

// Registry is the Server-side source of truth for the Herd.
// The zero value is not usable; construct with NewRegistry.
// All methods are safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	panes []Pane
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// ListPanes returns a snapshot of every Pane in the Herd, in stable order.
// The returned slice is a copy; callers may retain or mutate it freely.
func (r *Registry) ListPanes() []Pane {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Pane, len(r.panes))
	copy(out, r.panes)
	return out
}
