// Package herd defines the domain vocabulary shared by the Server and the
// Client: the types both sides of the wire speak, named per CONTEXT.md.
// Later slices add Status and provenance here.
//
// Server-only machinery (the Pane registry, tmux control-mode ingestion)
// lives in internal/server; this package stays dependency-free so any
// package may import it.
package herd

// Pane is one Agent's terminal surface, owned by tmux and interpreted by
// the Server (see CONTEXT.md).
type Pane struct {
	// ID is the Server-scoped stable identifier of the Pane.
	ID string

	// Title is a human-readable label for dashboards.
	Title string
}
