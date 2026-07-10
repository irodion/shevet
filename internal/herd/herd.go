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

// PaneRef is the Client-scoped identity of a Pane: a Host alias plus the
// Server-scoped pane id. A tmux pane id (%0, %1) is only unique within one
// Host, so the same id recurs across Hosts; the Client therefore names every
// Pane by a PaneRef — for selection, focus, input routing, and telemetry
// aggregation — never by a bare pane id (ARCHITECTURE §3.2).
//
// A PaneRef is assembled Client-side and never crosses the wire: each gRPC
// connection is exactly one Host, so Server-scoped ids stay unambiguous per
// channel and Pane messages carry no host field. Host aliases are unique
// within one Client invocation, which makes a PaneRef unique across the Herd.
type PaneRef struct {
	// Host is the alias of the Host whose Server owns the Pane.
	Host string

	// ID is the Server-scoped pane id (tmux's %N), unique per Host.
	ID string
}

// String renders the PaneRef as "host:%id" — its stable form for map keys,
// logs, and diagnostics.
func (r PaneRef) String() string {
	return r.Host + ":" + r.ID
}
