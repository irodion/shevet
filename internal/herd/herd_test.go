package herd

import "testing"

func TestNewRegistry_IsEmpty(t *testing.T) {
	r := NewRegistry()
	if panes := r.ListPanes(); len(panes) != 0 {
		t.Errorf("new Registry has %d panes, want 0", len(panes))
	}
}

func TestListPanes_ReturnsACopy(t *testing.T) {
	r := NewRegistry()
	r.panes = []Pane{{ID: "%1", Title: "agent-a"}}

	snapshot := r.ListPanes()
	snapshot[0].Title = "mutated"

	if r.panes[0].Title != "agent-a" {
		t.Error("mutating the ListPanes result reached the Registry")
	}
}
