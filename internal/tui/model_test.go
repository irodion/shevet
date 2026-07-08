package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
)

type fakeStream struct {
	updates []client.PaneUpdate
	err     error
}

func (f *fakeStream) Recv() (client.PaneUpdate, error) {
	if len(f.updates) == 0 {
		if f.err != nil {
			return client.PaneUpdate{}, f.err
		}
		return client.PaneUpdate{}, errors.New("fake stream drained")
	}
	u := f.updates[0]
	f.updates = f.updates[1:]
	return u, nil
}

type fakeConn struct {
	panes    []herd.Pane
	err      error
	stream   *fakeStream
	watchErr error

	watched []string
}

func (f *fakeConn) ListPanes(context.Context) ([]herd.Pane, error) {
	return f.panes, f.err
}

func (f *fakeConn) WatchPane(_ context.Context, paneID string) (PaneStream, error) {
	f.watched = append(f.watched, paneID)
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	return f.stream, nil
}

// pump runs one command and folds its message into the model, the way the
// Bubble Tea runtime would, returning the next command.
func pump(t *testing.T, m Model, cmd tea.Cmd) (Model, tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command to run, got nil")
	}
	updated, next := m.Update(cmd())
	model, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", updated)
	}
	return model, next
}

// loadedModel runs the fetch cycle (Init cmd -> resulting msg -> Update).
func loadedModel(t *testing.T, conn Conn) (Model, tea.Cmd) {
	t.Helper()
	m := New(context.Background(), conn)
	return pump(t, m, m.Init())
}

func viewContent(m Model) string {
	return m.View().Content
}

func TestView_EmptyHerd(t *testing.T) {
	m, _ := loadedModel(t, &fakeConn{})
	if got := viewContent(m); !strings.Contains(got, "0 Panes") {
		t.Errorf("view does not show empty Herd:\n%s", got)
	}
}

func TestView_FetchError(t *testing.T) {
	m, _ := loadedModel(t, &fakeConn{err: errors.New("socket vanished")})
	if got := viewContent(m); !strings.Contains(got, "socket vanished") {
		t.Errorf("view does not surface the fetch error:\n%s", got)
	}
}

func TestView_BeforeLoad(t *testing.T) {
	m := New(context.Background(), &fakeConn{})
	if got := viewContent(m); !strings.Contains(got, "Connecting") {
		t.Errorf("view does not show the connecting state:\n%s", got)
	}
}

func TestView_UsesAltScreen(t *testing.T) {
	if !New(context.Background(), &fakeConn{}).View().AltScreen {
		t.Error("dashboard does not request the alternate screen")
	}
}

func TestUpdate_QuitKeys(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: 'q', Text: "q"},
		{Code: 'c', Mod: tea.ModCtrl},
	} {
		_, cmd := New(context.Background(), &fakeConn{}).Update(key)
		if cmd == nil {
			t.Errorf("key %q did not produce a command", tea.Key(key).String())
			continue
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("key %q produced %T, want tea.QuitMsg", tea.Key(key).String(), cmd())
		}
	}
}

// watchingModel drives the full cycle: fetch -> watch first pane -> stream
// consumed to exhaustion of the given updates.
func watchingModel(t *testing.T, updates []client.PaneUpdate) (Model, *fakeConn) {
	t.Helper()
	conn := &fakeConn{
		panes:  []herd.Pane{{ID: "%1", Title: "agent-a"}, {ID: "%2", Title: "agent-b"}},
		stream: &fakeStream{updates: updates},
	}

	m, cmd := loadedModel(t, conn) // fetch -> watchPaneCmd
	m, cmd = pump(t, m, cmd)       // watchStartedMsg -> recvCmd
	for range updates {
		m, cmd = pump(t, m, cmd) // paneUpdateMsg -> recvCmd (or stop)
	}
	return m, conn
}

func TestWatch_PicksTheFirstPane(t *testing.T) {
	_, conn := watchingModel(t, nil)
	if len(conn.watched) != 1 || conn.watched[0] != "%1" {
		t.Errorf("watched panes = %v, want [%%1]", conn.watched)
	}
}

func TestWatch_RendersDamage(t *testing.T) {
	m, _ := watchingModel(t, []client.PaneUpdate{
		{Resized: &grid.Size{W: 10, H: 3}},
		{
			Damage: []grid.CellPatch{
				{X: 0, Y: 0, Cell: grid.Cell{Content: "h", Width: 1}},
				{X: 1, Y: 0, Cell: grid.Cell{Content: "i", Width: 1}},
			},
			Cursor: grid.Cursor{X: 2, Y: 0},
		},
	})

	got := viewContent(m)
	if !strings.Contains(got, "hi") {
		t.Errorf("view does not render the Pane content:\n%q", got)
	}
	if strings.Contains(got, "Shevet") {
		t.Errorf("pane view still shows dashboard chrome:\n%q", got)
	}
}

func TestWatch_ResizeResetsContent(t *testing.T) {
	m, _ := watchingModel(t, []client.PaneUpdate{
		{Resized: &grid.Size{W: 10, H: 3}},
		{
			Damage: []grid.CellPatch{{X: 0, Y: 0, Cell: grid.Cell{Content: "X", Width: 1}}},
			Cursor: grid.Cursor{X: 1, Y: 0},
		},
		{Resized: &grid.Size{W: 5, H: 2}},
	})

	if got := viewContent(m); strings.Contains(got, "X") {
		t.Errorf("content survived a resize, want a reset grid:\n%q", got)
	}
}

func TestWatch_PaneExit(t *testing.T) {
	m, _ := watchingModel(t, []client.PaneUpdate{
		{Resized: &grid.Size{W: 10, H: 3}},
		{Exited: true},
	})

	if got := viewContent(m); !strings.Contains(got, "exited") {
		t.Errorf("view does not report the Pane exit:\n%q", got)
	}
}

func TestWatch_StreamErrorSurfaces(t *testing.T) {
	conn := &fakeConn{
		panes:  []herd.Pane{{ID: "%1"}},
		stream: &fakeStream{err: errors.New("stream torn")},
	}
	m, cmd := loadedModel(t, conn)
	m, cmd = pump(t, m, cmd) // watchStartedMsg
	m, _ = pump(t, m, cmd)   // recv -> watchFailedMsg

	if got := viewContent(m); !strings.Contains(got, "stream torn") {
		t.Errorf("view does not surface the stream error:\n%q", got)
	}
}
