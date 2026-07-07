// Package wire translates between the shared domain types (internal/herd)
// and their shevet.v1 protocol representations.
//
// Every message has exactly one mapping per direction, and both directions
// live side by side so a new proto field is added to both at once — the
// Pane message is declared additive (Status, provenance, geometry arrive in
// later slices), and a mapping updated on only one side silently drops data
// on one direction of the wire.
package wire

import (
	"github.com/irodion/shevet/internal/herd"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// PaneToProto converts a domain Pane to its wire representation.
func PaneToProto(p herd.Pane) *shevetv1.Pane {
	return &shevetv1.Pane{
		Id:    p.ID,
		Title: p.Title,
	}
}

// PaneFromProto converts a wire Pane to its domain representation.
func PaneFromProto(p *shevetv1.Pane) herd.Pane {
	return herd.Pane{
		ID:    p.GetId(),
		Title: p.GetTitle(),
	}
}
