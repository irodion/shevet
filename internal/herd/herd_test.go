package herd

import "testing"

func TestPaneRef_String(t *testing.T) {
	ref := PaneRef{Host: "prod", ID: "%3"}
	if got, want := ref.String(), "prod:%3"; got != want {
		t.Errorf("PaneRef.String() = %q, want %q", got, want)
	}
}

// TestPaneRef_DistinctAcrossHosts pins the reason PaneRef exists: the same
// tmux pane id on two Hosts must be two different identities.
func TestPaneRef_DistinctAcrossHosts(t *testing.T) {
	a := PaneRef{Host: "alpha", ID: "%0"}
	b := PaneRef{Host: "beta", ID: "%0"}
	if a == b {
		t.Fatalf("panes sharing a tmux id across Hosts compared equal: %s", a)
	}
}
