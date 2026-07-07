package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketRel_IsRelative(t *testing.T) {
	if rel := SocketRel(); filepath.IsAbs(rel) {
		t.Errorf("SocketRel() = %q, want a home-relative path", rel)
	}
}

func TestDefaultSocket_ResolvesConventionAgainstHome(t *testing.T) {
	got, err := DefaultSocket()
	if err != nil {
		t.Fatalf("DefaultSocket: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("DefaultSocket() = %q, want an absolute path", got)
	}
	if !strings.HasSuffix(got, SocketRel()) {
		t.Errorf("DefaultSocket() = %q, want suffix %q", got, SocketRel())
	}
}
