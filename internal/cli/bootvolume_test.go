package cli

import "testing"

func TestIsNonBootVolumePath(t *testing.T) {
	cases := []struct {
		name string
		goos string
		path string
		want bool
	}{
		{"macos external volume", "darwin", "/Volumes/MacWork/projects/shevet/bin/shevet", true},
		{"macos disk image", "darwin", "/Volumes/Installer/shevet", true},
		{"macos boot volume", "darwin", "/usr/local/bin/shevet", false},
		{"macos home", "darwin", "/Users/dev/bin/shevet", false},
		{"macos volumes lookalike prefix", "darwin", "/VolumesExtra/shevet", false},
		{"linux mnt", "linux", "/Volumes/MacWork/shevet", false},
		{"linux media", "linux", "/media/usb/shevet", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNonBootVolumePath(tc.goos, tc.path); got != tc.want {
				t.Errorf("isNonBootVolumePath(%q, %q) = %v, want %v", tc.goos, tc.path, got, tc.want)
			}
		})
	}
}
