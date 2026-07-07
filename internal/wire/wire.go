// Package wire translates between the shared domain types (internal/herd,
// internal/grid) and their shevet.v1 protocol representations.
//
// Every message has exactly one mapping per direction, and both directions
// live side by side so a new proto field is added to both at once — the
// Pane message is declared additive (Status, provenance, geometry arrive in
// later slices), and a mapping updated on only one side silently drops data
// on one direction of the wire.
package wire

import (
	"github.com/irodion/shevet/internal/grid"
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

// PatchToProto converts a cell patch to its wire representation.
func PatchToProto(p grid.CellPatch) *shevetv1.CellPatch {
	return &shevetv1.CellPatch{
		X:       uint32(p.X),
		Y:       uint32(p.Y),
		Content: p.Cell.Content,
		Width:   uint32(p.Cell.Width),
		Fg:      uint32(p.Cell.FG),
		Bg:      uint32(p.Cell.BG),
		Attrs:   uint32(p.Cell.Attrs),
	}
}

// PatchFromProto converts a wire cell patch to its domain representation.
func PatchFromProto(p *shevetv1.CellPatch) grid.CellPatch {
	return grid.CellPatch{
		X: int(p.GetX()),
		Y: int(p.GetY()),
		Cell: grid.Cell{
			Content: p.GetContent(),
			Width:   int(p.GetWidth()),
			FG:      grid.Color(p.GetFg()),
			BG:      grid.Color(p.GetBg()),
			Attrs:   grid.Attr(p.GetAttrs()),
		},
	}
}

// CursorToProto converts a cursor to its wire representation.
func CursorToProto(c grid.Cursor) *shevetv1.Cursor {
	return &shevetv1.Cursor{
		X:      uint32(c.X),
		Y:      uint32(c.Y),
		Hidden: c.Hidden,
	}
}

// CursorFromProto converts a wire cursor to its domain representation.
func CursorFromProto(c *shevetv1.Cursor) grid.Cursor {
	return grid.Cursor{
		X:      int(c.GetX()),
		Y:      int(c.GetY()),
		Hidden: c.GetHidden(),
	}
}
