package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/irodion/shevet/internal/herd"
)

type fakeLister struct {
	panes []herd.Pane
	err   error
}

func (f fakeLister) ListPanes(context.Context) ([]herd.Pane, error) {
	return f.panes, f.err
}

// loadedModel runs the fetch cycle (Init cmd -> resulting msg -> Update) the
// way the Bubble Tea runtime would.
func loadedModel(t *testing.T, lister PaneLister) Model {
	t.Helper()
	m := New(lister)

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil cmd, want a fetch")
	}
	updated, _ := m.Update(cmd())

	model, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", updated)
	}
	return model
}

func viewContent(m Model) string {
	return m.View().Content
}

func TestView_EmptyHerd(t *testing.T) {
	m := loadedModel(t, fakeLister{})
	if got := viewContent(m); !strings.Contains(got, "0 Panes") {
		t.Errorf("view does not show empty Herd:\n%s", got)
	}
}

func TestView_SingularPane(t *testing.T) {
	m := loadedModel(t, fakeLister{panes: []herd.Pane{{ID: "%1", Title: "agent-a"}}})

	got := viewContent(m)
	if !strings.Contains(got, "1 Pane") || strings.Contains(got, "1 Panes") {
		t.Errorf("view does not pluralize correctly:\n%s", got)
	}
	if !strings.Contains(got, "agent-a") {
		t.Errorf("view does not list the Pane title:\n%s", got)
	}
}

func TestView_FetchError(t *testing.T) {
	m := loadedModel(t, fakeLister{err: errors.New("socket vanished")})
	if got := viewContent(m); !strings.Contains(got, "socket vanished") {
		t.Errorf("view does not surface the fetch error:\n%s", got)
	}
}

func TestView_BeforeLoad(t *testing.T) {
	m := New(fakeLister{})
	if got := viewContent(m); !strings.Contains(got, "Connecting") {
		t.Errorf("view does not show the connecting state:\n%s", got)
	}
}

func TestView_UsesAltScreen(t *testing.T) {
	if !New(fakeLister{}).View().AltScreen {
		t.Error("dashboard does not request the alternate screen")
	}
}

func TestUpdate_QuitKeys(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: 'q', Text: "q"},
		{Code: 'c', Mod: tea.ModCtrl},
	} {
		_, cmd := New(fakeLister{}).Update(key)
		if cmd == nil {
			t.Errorf("key %q did not produce a command", tea.Key(key).String())
			continue
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("key %q produced %T, want tea.QuitMsg", tea.Key(key).String(), cmd())
		}
	}
}

func TestUpdate_TracksWindowSize(t *testing.T) {
	updated, _ := New(fakeLister{}).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := updated.(Model)
	if m.width != 80 || m.height != 24 {
		t.Errorf("window size = %dx%d, want 80x24", m.width, m.height)
	}
}
