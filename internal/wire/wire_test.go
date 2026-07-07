package wire

import (
	"testing"

	"github.com/irodion/shevet/internal/herd"
)

// TestPane_RoundTrip guards the two mapping directions against drifting
// apart: every domain field must survive domain -> proto -> domain.
func TestPane_RoundTrip(t *testing.T) {
	in := herd.Pane{ID: "%1", Title: "agent-a"}

	if got := PaneFromProto(PaneToProto(in)); got != in {
		t.Errorf("round trip mangled the Pane: got %+v, want %+v", got, in)
	}
}
