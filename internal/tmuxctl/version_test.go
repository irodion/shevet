package tmuxctl

import "testing"

// TestParseVersion pins the tmux version gate (ADR-0008): a parseable
// major.minor decides the floor, and anything unparseable (development builds)
// is deliberately allowed through rather than blocking an upgrade.
func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"3.2", 3, 2, true},
		{"3.2a", 3, 2, true}, // OpenBSD portable suffix
		{"3.7b", 3, 7, true},
		{"3.1c", 3, 1, true},
		{"2.9", 2, 9, true},
		{"3.10", 3, 10, true}, // minor is multi-digit, not lexical
		{"next-3.4", 3, 4, true},
		{"master", 0, 0, false},
		{"", 0, 0, false},
		{"3", 0, 0, false}, // no minor
		{"x.y", 0, 0, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			major, minor, ok := parseVersion(tc.in)
			if ok != tc.ok || (ok && (major != tc.major || minor != tc.minor)) {
				t.Errorf("parseVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
			}
		})
	}
}

// TestVersionFloorRejectsOldReleases guards the production comparison itself:
// every pre-3.2 release must be refused and every 3.2+ release accepted.
func TestVersionFloorRejectsOldReleases(t *testing.T) {
	for _, tc := range []struct {
		major, minor int
		reject       bool
	}{
		{3, 1, true},
		{3, 0, true},
		{2, 9, true},
		{3, 2, false},
		{3, 3, false},
		{3, 10, false},
		{4, 0, false},
	} {
		if got := belowFloor(tc.major, tc.minor); got != tc.reject {
			t.Errorf("belowFloor(%d.%d) = %v, want reject=%v", tc.major, tc.minor, got, tc.reject)
		}
	}
}
