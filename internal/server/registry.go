package server

import (
	"sync"

	"github.com/irodion/shevet/internal/herd"
)

// Registry is the Server-side source of truth for which Panes exist in the
// Herd. In this slice it is an empty, concurrency-safe shell; later slices
// populate it from tmux control-mode events and add the Spawn/Adopt
// lifecycle.
//
// The zero value is not usable; construct with NewRegistry. All methods are
// safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	panes []herd.Pane
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// ListPanes returns a snapshot of every Pane in the Herd, in stable order.
// The returned slice is a copy; callers may retain or mutate it freely.
func (r *Registry) ListPanes() []herd.Pane {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]herd.Pane, len(r.panes))
	copy(out, r.panes)
	return out
}
