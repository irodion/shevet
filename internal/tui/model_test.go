package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

	sink     *fakeSink
	inputErr error

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

func (f *fakeConn) SendInput(context.Context) (InputSink, error) {
	if f.inputErr != nil {
		return nil, f.inputErr
	}
	if f.sink == nil {
		f.sink = &fakeSink{}
	}
	return f.sink, nil
}

// fakeSink records the keystrokes forwarded through it, guarded because the
// forwarder writes from its own goroutine while the test reads.
type fakeSink struct {
	mu      sync.Mutex
	pane    string
	keys    []byte
	sendErr error
}

func (s *fakeSink) SendKeys(paneID string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.pane = paneID
	s.keys = append(s.keys, data...)
	return nil
}

// received returns the bytes forwarded so far.
func (s *fakeSink) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.keys...)
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
		// Pre-created so the pointer is stable while the forwarding Cmd's
		// goroutine writes through it (the fakeSink's mutex guards the data).
		sink: &fakeSink{},
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

// liveModel watches %1 with a single resize update, leaving a read-only view
// of a sized, live Pane — the state passthrough is entered from.
func liveModel(t *testing.T) (Model, *fakeConn) {
	t.Helper()
	return watchingModel(t, []client.PaneUpdate{{Resized: &grid.Size{W: 10, H: 3}}})
}

// enterFocus presses 'i' and runs the forwarding Cmd on its own goroutine, as
// the Bubble Tea runtime would — leaving a model in passthrough whose
// keystrokes drain to the fake sink.
func enterFocus(t *testing.T, m Model) Model {
	t.Helper()
	next, cmd := m.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	m = next.(Model)
	if !m.focused {
		t.Fatal("pressing 'i' on a live Pane did not enter passthrough")
	}
	if cmd == nil || m.input == nil {
		t.Fatal("entering passthrough did not open the input stream")
	}
	go cmd() // opens the stream and drains m.input
	return m
}

// press feeds one key and returns the updated model and command.
func press(t *testing.T, m Model, key tea.KeyPressMsg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(key)
	return next.(Model), cmd
}

// waitKeys waits for the sink to have received exactly want.
func waitKeys(t *testing.T, sink *fakeSink, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if string(sink.received()) == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("forwarded %q, want %q", sink.received(), want)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestFocus_ForwardsEncodedKeystrokes(t *testing.T) {
	m, conn := liveModel(t)
	m = enterFocus(t, m)

	// Type a mix that crosses the encoder's paths: text, Enter, an arrow.
	for _, k := range []tea.KeyPressMsg{
		{Code: 'h', Text: "h"},
		{Code: 'i', Text: "i"},
		{Code: tea.KeyEnter},
		{Code: tea.KeyUp},
	} {
		m, _ = press(t, m, k)
	}
	waitKeys(t, conn.sink, "hi\r\x1b[A")
	if conn.sink.pane != "%1" {
		t.Errorf("forwarded to pane %q, want %%1", conn.sink.pane)
	}
}

func TestFocus_CtrlCForwardsInsteadOfQuitting(t *testing.T) {
	m, conn := liveModel(t)
	m = enterFocus(t, m)

	_, cmd := press(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("Ctrl-C quit the dashboard while in passthrough, want it forwarded")
		}
	}
	waitKeys(t, conn.sink, "\x03")
}

func TestFocus_LeaderReturnsToReadOnly(t *testing.T) {
	m, _ := liveModel(t)
	m = enterFocus(t, m)

	m, _ = press(t, m, tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl}) // Ctrl-\
	if m.focused {
		t.Fatal("the leader did not return from passthrough")
	}
	// Back in read-only, 'q' quits again.
	_, cmd := press(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("'q' produced no command in read-only")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("'q' produced %T, want tea.QuitMsg", cmd())
	}
}

func TestFocus_QueuesKeystrokesUntilStreamOpens(t *testing.T) {
	m, conn := liveModel(t)

	// Enter passthrough, but hold the forwarding Cmd (which opens the stream
	// and drains) unstarted.
	m, openCmd := press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	if openCmd == nil || m.input == nil {
		t.Fatal("entering passthrough did not open the input stream")
	}

	// A keystroke typed before the stream is ready must be buffered, not lost.
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if len(m.input) != 1 {
		t.Fatalf("buffered %d keystrokes, want 1 held before the stream opened", len(m.input))
	}

	// Once the forwarder runs, it opens the stream and drains the buffer.
	go openCmd()
	waitKeys(t, conn.sink, "x")
}

func TestFocus_UnavailableWhenInputStreamFails(t *testing.T) {
	conn := &fakeConn{
		panes:    []herd.Pane{{ID: "%1"}},
		stream:   &fakeStream{updates: []client.PaneUpdate{{Resized: &grid.Size{W: 10, H: 3}}}},
		inputErr: errors.New("input stream refused"),
	}
	m, cmd := loadedModel(t, conn)
	m, cmd = pump(t, m, cmd) // watchStarted -> recv
	m, _ = pump(t, m, cmd)   // resize update

	m, cmd = press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	next, _ := m.Update(cmd()) // inputFailedMsg
	m = next.(Model)

	if m.focused {
		t.Error("stayed in passthrough despite the input stream failing")
	}
	if m.inputErr == nil {
		t.Error("input failure was not recorded")
	}
	// 'q' quits again, since passthrough dropped back to read-only.
	_, cmd = press(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("read-only 'q' no longer quits after input failure")
	}
}

func TestFocus_RetriesAfterInputFailure(t *testing.T) {
	conn := &fakeConn{
		panes:    []herd.Pane{{ID: "%1"}},
		stream:   &fakeStream{updates: []client.PaneUpdate{{Resized: &grid.Size{W: 10, H: 3}}}},
		sink:     &fakeSink{},
		inputErr: errors.New("input stream refused"),
	}
	m, cmd := loadedModel(t, conn)
	m, cmd = pump(t, m, cmd) // watchStarted -> recv
	m, _ = pump(t, m, cmd)   // resize update

	// First attempt fails and drops back to read-only.
	m, cmd = press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	next, _ := m.Update(cmd()) // inputFailedMsg
	m = next.(Model)
	if m.focused || m.inputErr == nil {
		t.Fatalf("after failure want read-only with a recorded error, got focused=%v err=%v", m.focused, m.inputErr)
	}

	// The Server recovers; re-entering passthrough retries the stream and
	// forwards again, rather than swallowing keys forever.
	conn.inputErr = nil
	m = enterFocus(t, m)
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'z', Text: "z"})
	waitKeys(t, conn.sink, "z")
	if m.inputErr != nil {
		t.Errorf("inputErr not cleared on retry: %v", m.inputErr)
	}
}

func TestFocus_IgnoredWithoutALivePane(t *testing.T) {
	// Empty Herd: nothing to type into, so 'i' must not enter passthrough.
	m, _ := loadedModel(t, &fakeConn{})
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	if m.focused {
		t.Error("entered passthrough with no live Pane on screen")
	}
	if cmd != nil {
		t.Errorf("opened an input stream with no Pane, cmd=%T", cmd())
	}
}
