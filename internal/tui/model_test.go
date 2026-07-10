package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
	"github.com/irodion/shevet/internal/testutil"
)

// fakeStream is a scripted render stream. Once the script is drained, Recv
// blocks on park when set (a live Pane with nothing new to say — what the
// golden and runtime tests need) or reports err / a drained error otherwise.
type fakeStream struct {
	mu      sync.Mutex
	updates []client.PaneUpdate
	err     error
	park    chan struct{}
}

func (f *fakeStream) Recv() (client.PaneUpdate, error) {
	f.mu.Lock()
	if len(f.updates) > 0 {
		u := f.updates[0]
		f.updates = f.updates[1:]
		f.mu.Unlock()
		return u, nil
	}
	err := f.err
	park := f.park
	f.mu.Unlock()

	if park != nil {
		<-park
	}
	if err != nil {
		return client.PaneUpdate{}, err
	}
	return client.PaneUpdate{}, errors.New("fake stream drained")
}

// fakeConn is a scripted Server connection. Methods are guarded because the
// runtime-driven tests (teatest) call them from command goroutines.
type fakeConn struct {
	mu       sync.Mutex
	panes    []herd.Pane
	err      error
	streams  map[string]*fakeStream
	watchErr error

	sink     *fakeSink
	inputErr error

	watched []string
}

func (f *fakeConn) ListPanes(context.Context) ([]herd.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.panes, f.err
}

func (f *fakeConn) WatchPane(_ context.Context, paneID string) (PaneStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched = append(f.watched, paneID)
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	if s := f.streams[paneID]; s != nil {
		return s, nil
	}
	return &fakeStream{}, nil
}

func (f *fakeConn) SendInput(context.Context) (InputSink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputErr != nil {
		return nil, f.inputErr
	}
	if f.sink == nil {
		f.sink = &fakeSink{}
	}
	return f.sink, nil
}

func (f *fakeConn) watchedPanes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.watched...)
}

// resizeCall is one recorded SendResize.
type resizeCall struct {
	pane string
	size grid.Size
}

// fakeSink records the requests forwarded through it, guarded because the
// forwarding Cmd writes from its own goroutine while the test reads.
type fakeSink struct {
	mu       sync.Mutex
	pane     string
	keys     []byte
	resizes  []resizeCall
	restores []string
	sendErr  error
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

func (s *fakeSink) SendResize(paneID string, w, h int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.resizes = append(s.resizes, resizeCall{pane: paneID, size: grid.Size{W: w, H: h}})
	return nil
}

func (s *fakeSink) SendRestore(paneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.restores = append(s.restores, paneID)
	return nil
}

// restored returns the restore requests forwarded so far.
func (s *fakeSink) restored() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.restores...)
}

// received returns the key bytes forwarded so far.
func (s *fakeSink) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.keys...)
}

// resized returns the resize requests forwarded so far.
func (s *fakeSink) resized() []resizeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resizeCall(nil), s.resizes...)
}

// testAlias is the Host alias every single-connection test scopes its Panes
// under; PaneRefs in those tests are testAlias:<pane id>.
const testAlias = "h"

func testRef(paneID string) herd.PaneRef {
	return herd.PaneRef{Host: testAlias, ID: paneID}
}

// single wraps one connection as the sole Server in a Client invocation.
func single(conn Conn) []Server {
	return []Server{{Alias: testAlias, Conn: conn}}
}

// pump runs one command and folds its message into the model, the way the
// Bubble Tea runtime would, returning the next command.
func pump(t *testing.T, m Model, cmd tea.Cmd) (Model, tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command to run, got nil")
	}
	return feed(t, m, cmd())
}

// feed folds one message into the model.
func feed(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	updated, next := m.Update(msg)
	model, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", updated)
	}
	return model, next
}

// loadedModel runs the fetch cycle (Init cmd -> resulting msg -> Update),
// returning the model plus the batched watch commands.
func loadedModel(t *testing.T, conn Conn) (Model, tea.Cmd) {
	t.Helper()
	m := New(context.Background(), single(conn))
	return pump(t, m, m.Init())
}

// runWatches executes the watch commands a Herd load batches — the
// runtime's fan-out, inlined so tests stay deterministic — feeding each
// watchStartedMsg back into the model and discarding the follow-up receive
// loops (tests feed pane updates directly instead).
func runWatches(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	cmds, ok := msg.(tea.BatchMsg)
	if !ok {
		m, _ = feed(t, m, msg)
		return m
	}
	for _, c := range cmds {
		m, _ = feed(t, m, c())
	}
	return m
}

// update feeds one pane update straight into the model, as the receive loop
// would deliver it — tagged with the Pane's current stream, the way a real
// recvCmd message is.
func update(t *testing.T, m Model, paneID string, u client.PaneUpdate) Model {
	t.Helper()
	m, _ = feed(t, m, paneUpdateMsg{ref: testRef(paneID), stream: currentStream(m, testRef(paneID)), update: u})
	return m
}

// currentStream is the stream the model believes feeds ref (nil when the
// Pane is unknown), for building messages that must pass the staleness check.
func currentStream(m Model, ref herd.PaneRef) PaneStream {
	if st := m.views[ref]; st != nil {
		return st.stream
	}
	return nil
}

// sizeMsg gives the dashboard its viewport, as the runtime does on start.
func sizeMsg(t *testing.T, m Model, w, h int) Model {
	t.Helper()
	m, _ = feed(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func viewContent(m Model) string {
	return m.View().Content
}

// resize and hello are the canonical first updates of a Pane's stream.
func resize(w, h int) client.PaneUpdate {
	return client.PaneUpdate{Resized: &grid.Size{W: w, H: h}}
}

func textUpdate(text string, cursorX, cursorY int) client.PaneUpdate {
	patches := make([]grid.CellPatch, 0, len(text))
	for i, r := range []rune(text) {
		patches = append(patches, grid.CellPatch{X: i, Y: 0, Cell: grid.Cell{Content: string(r), Width: 1}})
	}
	return client.PaneUpdate{Damage: patches, Cursor: grid.Cursor{X: cursorX, Y: cursorY}}
}

// twoPaneModel builds the standard fixture: a loaded dashboard on a 120x30
// viewport (wide enough for two card columns), both Panes watched and live
// at 20x5 with known content.
func twoPaneModel(t *testing.T) (Model, *fakeConn) {
	t.Helper()
	conn := &fakeConn{
		panes: []herd.Pane{{ID: "%1", Title: "agent-a"}, {ID: "%2", Title: "agent-b"}},
		// Pre-created so the pointer is stable while the forwarding Cmd's
		// goroutine writes through it (the fakeSink's mutex guards the data).
		sink: &fakeSink{},
	}
	m, cmd := loadedModel(t, conn)
	m = runWatches(t, m, cmd)
	m = sizeMsg(t, m, 120, 30)
	m = update(t, m, "%1", resize(20, 5))
	m = update(t, m, "%1", textUpdate("alpha says hi", 0, 1))
	m = update(t, m, "%2", resize(20, 5))
	m = update(t, m, "%2", textUpdate("beta waits", 0, 1))
	return m, conn
}

func TestView_EmptyHerd(t *testing.T) {
	m, _ := loadedModel(t, &fakeConn{})
	m = sizeMsg(t, m, 80, 24)
	got := viewContent(m)
	if !strings.Contains(got, "0 Panes") {
		t.Errorf("view does not show the empty Herd count:\n%s", got)
	}
	if !strings.Contains(got, "Herd is empty") {
		t.Errorf("view does not explain the empty state:\n%s", got)
	}
}

func TestView_FetchError(t *testing.T) {
	m, _ := loadedModel(t, &fakeConn{err: errors.New("socket vanished")})
	if got := viewContent(m); !strings.Contains(got, "socket vanished") {
		t.Errorf("view does not surface the fetch error:\n%s", got)
	}
}

func TestView_BeforeLoad(t *testing.T) {
	m := New(context.Background(), single(&fakeConn{}))
	if got := viewContent(m); !strings.Contains(got, "Connecting") {
		t.Errorf("view does not show the connecting state:\n%s", got)
	}
}

func TestView_UsesAltScreen(t *testing.T) {
	if !New(context.Background(), single(&fakeConn{})).View().AltScreen {
		t.Error("dashboard does not request the alternate screen")
	}
}

func TestUpdate_QuitKeys(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: 'q', Text: "q"},
		{Code: 'c', Mod: tea.ModCtrl},
	} {
		_, cmd := New(context.Background(), single(&fakeConn{})).Update(key)
		if cmd == nil {
			t.Errorf("key %q did not produce a command", tea.Key(key).String())
			continue
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("key %q produced %T, want tea.QuitMsg", tea.Key(key).String(), cmd())
		}
	}
}

func TestHerd_WatchesEveryPane(t *testing.T) {
	m, conn := twoPaneModel(t)
	watched := conn.watchedPanes()
	if len(watched) != 2 || watched[0] != "%1" || watched[1] != "%2" {
		t.Errorf("watched panes = %v, want [%%1 %%2] — the grid watches the whole Herd", watched)
	}
	_ = m
}

func TestGrid_RendersLiveThumbnails(t *testing.T) {
	m, _ := twoPaneModel(t)
	got := viewContent(m)
	for _, want := range []string{"agent-a", "agent-b", "alpha says hi", "beta waits", "h:%1", "h:%2"} {
		if !strings.Contains(got, want) {
			t.Errorf("grid view is missing %q:\n%s", want, ansi.Strip(got))
		}
	}
}

func TestGrid_SelectionMoves(t *testing.T) {
	m, _ := twoPaneModel(t)
	if m.selected != 0 {
		t.Fatalf("initial selection = %d, want 0", m.selected)
	}

	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.selected != 1 {
		t.Fatalf("after right, selection = %d, want 1", m.selected)
	}
	// The edge is a wall, not a wrap.
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	if m.selected != 1 {
		t.Fatalf("selection wrapped past the edge to %d", m.selected)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.selected != 0 {
		t.Fatalf("after left, selection = %d, want 0", m.selected)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if m.selected != 0 {
		t.Fatalf("selection wrapped past the left edge to %d", m.selected)
	}
}

func TestRefresh_KeepsSelectionOnTheSamePane(t *testing.T) {
	m, _ := twoPaneModel(t)
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // select %2

	// The Herd changes shape: a new Pane appears ahead of %2.
	m, _ = feed(t, m, panesLoadedMsg([]refPane{
		{Host: testAlias, Pane: herd.Pane{ID: "%1", Title: "agent-a"}},
		{Host: testAlias, Pane: herd.Pane{ID: "%3", Title: "agent-c"}},
		{Host: testAlias, Pane: herd.Pane{ID: "%2", Title: "agent-b"}},
	}))
	if m.selected != 2 {
		t.Errorf("selection did not follow its Pane across a refresh: index %d", m.selected)
	}
	if got := m.panes[m.selected].Ref(); got != testRef("%2") {
		t.Errorf("selected ref = %s, want %s", got, testRef("%2"))
	}
}

func TestRefresh_DropsDepartedPanes(t *testing.T) {
	m, _ := twoPaneModel(t)
	m, _ = feed(t, m, panesLoadedMsg([]refPane{
		{Host: testAlias, Pane: herd.Pane{ID: "%2", Title: "agent-b"}},
	}))
	if len(m.views) != 1 || m.views[testRef("%2")] == nil {
		t.Errorf("views after refresh = %v, want only %s", m.views, testRef("%2"))
	}
	// A stale update for the departed Pane is dropped, not resurrected.
	m = update(t, m, "%1", textUpdate("ghost", 0, 0))
	if m.views[testRef("%1")] != nil {
		t.Error("an update resurrected a departed Pane")
	}
}

// press feeds one key and returns the updated model and command.
func press(t *testing.T, m Model, key tea.KeyPressMsg) (Model, tea.Cmd) {
	t.Helper()
	return feed(t, m, key)
}

// enterFocus presses enter and runs the forwarding Cmd on its own goroutine,
// as the Bubble Tea runtime would — leaving a model in passthrough whose
// requests drain to the fake sink.
func enterFocus(t *testing.T, m Model) Model {
	t.Helper()
	next, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next
	if !m.focused() {
		t.Fatal("pressing enter on a live Pane did not enter passthrough")
	}
	if cmd != nil {
		go cmd() // opens the input stream and drains the channel
	} else if m.inputs[m.focus.Host] == nil {
		t.Fatal("entering passthrough neither opened an input stream nor reused one")
	}
	return m
}

// waitKeys waits for the sink to have received exactly want (the sink is
// fed from the forwarding Cmd's goroutine, so assertions must poll).
func waitKeys(t *testing.T, sink *fakeSink, want string) {
	t.Helper()
	testutil.Eventually(t, fmt.Sprintf("forwarded keys %q", want), func() (bool, string) {
		got := string(sink.received())
		return got == want, fmt.Sprintf("forwarded %q", got)
	})
}

// waitResizes waits for the sink to have received exactly the given resizes.
func waitResizes(t *testing.T, sink *fakeSink, want ...resizeCall) {
	t.Helper()
	testutil.Eventually(t, "forwarded resizes", func() (bool, string) {
		got := sink.resized()
		return len(got) >= len(want), fmt.Sprintf("resizes so far: %v", got)
	})
	got := sink.resized()
	if len(got) != len(want) {
		t.Fatalf("resizes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resize %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// waitRestores waits for the sink to have received exactly the given
// restore requests.
func waitRestores(t *testing.T, sink *fakeSink, want ...string) {
	t.Helper()
	testutil.Eventually(t, "forwarded restores", func() (bool, string) {
		got := sink.restored()
		return len(got) >= len(want), fmt.Sprintf("restores so far: %v", got)
	})
	got := sink.restored()
	if len(got) != len(want) {
		t.Fatalf("restores = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restore %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFocus_ResizesPaneToViewportAndRestores(t *testing.T) {
	m, conn := twoPaneModel(t) // viewport 120x30, panes 20x5
	m = enterFocus(t, m)

	// Focus resizes the Pane to the Client viewport.
	waitResizes(t, conn.sink, resizeCall{pane: "%1", size: grid.Size{W: 120, H: 30}})

	// The Client's terminal grows while focused: the Pane follows.
	m = sizeMsg(t, m, 140, 40)
	waitResizes(t, conn.sink,
		resizeCall{pane: "%1", size: grid.Size{W: 120, H: 30}},
		resizeCall{pane: "%1", size: grid.Size{W: 140, H: 40}})

	// The leader returns to the grid and asks the Server for the restore —
	// no dimensions: the Server owns the pre-focus size.
	m, _ = press(t, m, tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl})
	if m.focused() {
		t.Fatal("the leader did not return from passthrough")
	}
	waitRestores(t, conn.sink, "%1")
}

func TestFocus_ShowsThePaneFullScreen(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = enterFocus(t, m)

	got := viewContent(m)
	if !strings.Contains(got, "alpha says hi") {
		t.Errorf("focus view does not show the Pane content:\n%q", got)
	}
	if strings.Contains(got, "Shevet") || strings.Contains(got, "agent-b") {
		t.Errorf("focus view still shows dashboard chrome:\n%q", got)
	}
}

func TestFocus_ForwardsEncodedKeystrokes(t *testing.T) {
	m, conn := twoPaneModel(t)
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // select %2
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
	conn.sink.mu.Lock()
	pane := conn.sink.pane
	conn.sink.mu.Unlock()
	if pane != "%2" {
		t.Errorf("forwarded to pane %q, want the focused %%2", pane)
	}
}

func TestFocus_CtrlCForwardsInsteadOfQuitting(t *testing.T) {
	m, conn := twoPaneModel(t)
	m = enterFocus(t, m)

	_, cmd := press(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("Ctrl-C quit the dashboard while in passthrough, want it forwarded")
		}
	}
	waitKeys(t, conn.sink, "\x03")
}

func TestFocus_LeaderReturnsToGridKeys(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = enterFocus(t, m)

	m, _ = press(t, m, tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl}) // Ctrl-\
	if m.focused() {
		t.Fatal("the leader did not return from passthrough")
	}
	// Back on the grid, 'q' quits again.
	_, cmd := press(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("'q' produced no command on the grid")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("'q' produced %T, want tea.QuitMsg", cmd())
	}
}

func TestFocus_QueuesRequestsUntilStreamOpens(t *testing.T) {
	m, conn := twoPaneModel(t)

	// Enter passthrough, but hold the forwarding Cmd (which opens the stream
	// and drains) unstarted.
	m, openCmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if openCmd == nil || m.inputs[testAlias] == nil {
		t.Fatal("entering passthrough did not open the input stream")
	}

	// The focus resize plus a keystroke wait in the buffer, not lost.
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if len(m.inputs[testAlias].ch) != 2 {
		t.Fatalf("buffered %d requests, want 2 held before the stream opened", len(m.inputs[testAlias].ch))
	}

	// Once the forwarder runs, it opens the stream and drains the buffer.
	go openCmd()
	waitKeys(t, conn.sink, "x")
	waitResizes(t, conn.sink, resizeCall{pane: "%1", size: grid.Size{W: 120, H: 30}})
}

func TestFocus_UnavailableWhenInputStreamFails(t *testing.T) {
	m, conn := twoPaneModel(t)
	conn.mu.Lock()
	conn.inputErr = errors.New("input stream refused")
	conn.mu.Unlock()

	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = feed(t, m, cmd()) // forwarding Cmd fails to open -> inputFailedMsg

	if m.focused() {
		t.Error("stayed in passthrough despite the input stream failing")
	}
	if m.inputErrs[testAlias] == nil {
		t.Error("input failure was not recorded")
	}
	if got := viewContent(m); !strings.Contains(got, "input unavailable") {
		t.Errorf("grid does not surface the input failure:\n%s", ansi.Strip(got))
	}
	// 'q' quits again, since passthrough dropped back to the grid.
	_, cmd = press(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("grid 'q' no longer quits after input failure")
	}
}

func TestFocus_RetriesAfterInputFailure(t *testing.T) {
	m, conn := twoPaneModel(t)
	conn.mu.Lock()
	conn.inputErr = errors.New("input stream refused")
	conn.mu.Unlock()

	// First attempt fails and drops back to the grid.
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = feed(t, m, cmd())
	if m.focused() || m.inputErrs[testAlias] == nil {
		t.Fatalf("after failure want the grid with a recorded error, got focused=%v err=%v", m.focused(), m.inputErrs[testAlias])
	}

	// The Server recovers; focusing again retries the stream and forwards,
	// rather than swallowing keys forever.
	conn.mu.Lock()
	conn.inputErr = nil
	conn.mu.Unlock()
	m = enterFocus(t, m)
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'z', Text: "z"})
	waitKeys(t, conn.sink, "z")
	if m.inputErrs[testAlias] != nil {
		t.Errorf("inputErr not cleared on retry: %v", m.inputErrs[testAlias])
	}
}

func TestFocus_IgnoredForExitedPane(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = update(t, m, "%1", client.PaneUpdate{Exited: true})

	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.focused() {
		t.Error("entered passthrough into an exited Pane")
	}
	if cmd != nil {
		t.Errorf("opened an input stream for an exited Pane, cmd=%T", cmd())
	}
}

func TestFocus_IgnoredWithoutALivePane(t *testing.T) {
	// Empty Herd: nothing to type into, so enter must not enter passthrough.
	m, _ := loadedModel(t, &fakeConn{})
	m = sizeMsg(t, m, 80, 24)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.focused() {
		t.Error("entered passthrough with no live Pane on screen")
	}
	if cmd != nil {
		t.Errorf("opened an input stream with no Pane, cmd=%T", cmd())
	}
}

func TestFocus_PaneExitReturnsToGrid(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = enterFocus(t, m)

	m = update(t, m, "%1", client.PaneUpdate{Exited: true})
	if m.focused() {
		t.Error("still in passthrough after the focused Pane exited")
	}
	if got := viewContent(m); !strings.Contains(got, "exited") {
		t.Errorf("the exited Pane's card wears no badge:\n%s", ansi.Strip(got))
	}
}

// failStream marks a Pane's render stream broken, as recvCmd would report it.
func failStream(t *testing.T, m Model, paneID string, err error) Model {
	t.Helper()
	ref := testRef(paneID)
	m, _ = feed(t, m, watchFailedMsg{ref: ref, stream: currentStream(m, ref), err: err})
	return m
}

func TestGrid_StreamFailureWearsBadge(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = failStream(t, m, "%1", errors.New("stream torn"))

	if got := viewContent(m); !strings.Contains(got, "stream lost") {
		t.Errorf("grid does not surface the broken stream:\n%s", ansi.Strip(got))
	}
}

func TestFocus_RefusedForABrokenStream(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = failStream(t, m, "%1", errors.New("stream torn"))

	// Focusing a stream-lost Pane would be typing into a frozen frame.
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.focused() {
		t.Error("entered passthrough into a Pane whose render stream is broken")
	}
	if cmd != nil {
		t.Errorf("opened an input stream for a broken Pane, cmd=%T", cmd())
	}
}

func TestRefresh_RewatchesABrokenStream(t *testing.T) {
	m, conn := twoPaneModel(t)
	m = failStream(t, m, "%1", errors.New("stream torn"))

	// r refetches the Herd; the broken Pane must get a fresh watch, not
	// stay "stream lost" forever.
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'r', Text: "r"})
	m, cmd = pump(t, m, cmd) // fetch -> panesLoadedMsg -> applyHerd
	m = runWatches(t, m, cmd)

	if got := conn.watchedPanes(); len(got) != 3 || got[2] != "%1" {
		t.Fatalf("watched panes after refresh = %v, want a re-watch of %%1", got)
	}
	if st := m.views[testRef("%1")]; st == nil || st.err != nil {
		t.Errorf("the broken Pane's state was not reset: %+v", st)
	}
}

func TestWatch_StaleStreamUpdateIsDropped(t *testing.T) {
	m, _ := twoPaneModel(t)

	// An update tagged with a stream that is not the Pane's current one —
	// the tail of a replaced watch — must never reach the grid.
	stale := &fakeStream{}
	m, _ = feed(t, m, paneUpdateMsg{ref: testRef("%1"), stream: stale, update: textUpdate("GHOST", 0, 2)})
	if got := viewContent(m); strings.Contains(got, "GHOST") {
		t.Error("a stale stream's damage reached the grid")
	}

	// Same for a stale failure: it must not mark the live stream broken.
	m, _ = feed(t, m, watchFailedMsg{ref: testRef("%1"), stream: stale, err: errors.New("old stream torn")})
	if st := m.views[testRef("%1")]; st.err != nil {
		t.Error("a stale stream's failure marked the live Pane broken")
	}
}

func TestWatch_ResizeResetsContent(t *testing.T) {
	m, _ := twoPaneModel(t)
	m = update(t, m, "%1", resize(30, 8))

	if got := viewContent(m); strings.Contains(got, "alpha says hi") {
		t.Errorf("content survived a resize, want a reset grid:\n%s", ansi.Strip(got))
	}
}

func TestRefresh_FailureKeepsTheDashboard(t *testing.T) {
	m, _ := twoPaneModel(t)
	m, _ = feed(t, m, loadFailedMsg{err: errors.New("host went away")})

	got := viewContent(m)
	if !strings.Contains(got, "agent-a") {
		t.Errorf("a failed refresh blanked the live dashboard:\n%s", ansi.Strip(got))
	}
	if !strings.Contains(got, "host went away") {
		t.Errorf("the failed refresh is not surfaced in the footer:\n%s", ansi.Strip(got))
	}
}

// TestFocus_RefocusCyclesAreRestorePerUnfocus pins the shape of the wire
// conversation across rapid focus cycles: each focus sends the viewport,
// each unfocus sends a bare restore. What size a restore lands on is the
// Server's business — the Client carries no size bookkeeping, so no
// interleaving of echoes and external resizes can confuse it.
func TestFocus_RefocusCyclesAreRestorePerUnfocus(t *testing.T) {
	m, conn := twoPaneModel(t) // pane %1 is 20x5, viewport 120x30
	m = enterFocus(t, m)
	m, _ = press(t, m, tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl})
	m = update(t, m, "%1", resize(120, 30)) // a focus resize echoing back, late
	m = enterFocus(t, m)
	_, _ = press(t, m, tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl})

	waitResizes(t, conn.sink,
		resizeCall{pane: "%1", size: grid.Size{W: 120, H: 30}}, // focus
		resizeCall{pane: "%1", size: grid.Size{W: 120, H: 30}}, // refocus
	)
	waitRestores(t, conn.sink, "%1", "%1")
}

func TestFocus_StalledInputStreamDropsToGridInsteadOfBlocking(t *testing.T) {
	m, _ := twoPaneModel(t)

	// Enter focus but never start the forwarding Cmd: the stream is
	// effectively stalled, and every request stays in the buffer.
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.focused() {
		t.Fatal("enter did not focus")
	}

	// Fill the queue past its depth. This must never block Update — a
	// frozen dashboard that cannot even quit is the failure mode — and
	// once the buffer overflows, the stream is declared stalled: back to
	// the grid, footer notice (the Server restores the Pane's size when
	// the dead stream ends).
	for i := 0; i <= forwardBuffer; i++ {
		m, _ = press(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	}
	if m.focused() {
		t.Error("still in passthrough after the input queue overflowed")
	}
	if m.inputErrs[testAlias] == nil {
		t.Error("the stalled stream was not surfaced")
	}
	// The dashboard is alive: q still quits.
	_, cmd := press(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("'q' produced no command after the stall")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("'q' does not quit after the stall")
	}
}

func TestFocus_RetiredGenerationCannotHauntItsReplacement(t *testing.T) {
	m, conn := twoPaneModel(t)

	// Generation 1: focus, hold its forwarder unstarted, and wedge it —
	// fill the queue until the stream is declared stalled and retired.
	m, gen1Cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if gen1Cmd == nil {
		t.Fatal("focus did not open an input generation")
	}
	gen1 := m.inputs[testAlias]
	for i := 0; i <= forwardBuffer; i++ {
		m, _ = press(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	}
	if m.focused() || m.inputs[testAlias] != nil {
		t.Fatal("overflow did not retire the generation")
	}

	// Generation 2: refocusing opens fresh plumbing.
	m = enterFocus(t, m)
	gen2 := m.inputs[testAlias]
	if gen2 == nil || gen2 == gen1 {
		t.Fatal("refocus did not open a fresh generation")
	}

	// The retired forwarder finally runs. Its context was canceled when it
	// was retired, so the hundreds of queued keystrokes must never replay
	// into the Pane — only generation 2's focus resize reaches the wire.
	if msg := gen1Cmd(); msg != nil {
		m, _ = feed(t, m, msg)
	}
	waitResizes(t, conn.sink, resizeCall{pane: "%1", size: grid.Size{W: 120, H: 30}})
	if got := string(conn.sink.received()); strings.Contains(got, "x") {
		t.Errorf("a retired generation replayed stale input: %q", got)
	}

	// And its death rattle is ignored: a stale failure must not tear down
	// the live generation.
	m, _ = feed(t, m, inputFailedMsg{host: testAlias, gen: gen1, err: errors.New("late failure")})
	if !m.focused() {
		t.Error("a stale input failure unfocused the live session")
	}
	if m.inputs[testAlias] != gen2 {
		t.Error("a stale input failure tore down the live generation")
	}
	if m.inputErrs[testAlias] != nil {
		t.Errorf("a stale input failure was recorded: %v", m.inputErrs[testAlias])
	}
}

func TestNewServers_RejectsDuplicateAliases(t *testing.T) {
	_, err := NewServers([]Server{
		{Alias: "prod", Conn: &fakeConn{}},
		{Alias: "prod", Conn: &fakeConn{}},
	})
	if err == nil {
		t.Fatal("duplicate alias accepted, want an error")
	}
	// The message must name the offending alias so the developer can fix the
	// invocation, not just "invalid".
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("error %q does not name the duplicate alias", err)
	}
}

func TestNewServers_RejectsEmptyAlias(t *testing.T) {
	if _, err := NewServers([]Server{{Alias: "", Conn: &fakeConn{}}}); err == nil {
		t.Fatal("empty alias accepted, want an error")
	}
}

// TestHerd_TwoServersShareIdsWithoutCollision is the multi-Host identity
// contract: two Servers each owning a pane %0 aggregate into one Client model
// as distinct PaneRefs, and watch/input route to the owning connection by the
// PaneRef's Host — never leaking across to the other Server that shares the id.
func TestHerd_TwoServersShareIdsWithoutCollision(t *testing.T) {
	newConn := func(title string) *fakeConn {
		return &fakeConn{
			panes: []herd.Pane{{ID: "%0", Title: title}},
			sink:  &fakeSink{},
		}
	}
	alpha, beta := newConn("on-alpha"), newConn("on-beta")

	servers, err := NewServers([]Server{{Alias: "alpha", Conn: alpha}, {Alias: "beta", Conn: beta}})
	if err != nil {
		t.Fatalf("NewServers: %v", err)
	}

	m := New(context.Background(), servers)
	m, cmd := pump(t, m, m.Init()) // fetch both Hosts -> aggregated Herd -> watch all
	m = runWatches(t, m, cmd)
	m = sizeMsg(t, m, 80, 24)

	// Both %0 panes coexist under distinct PaneRefs.
	if len(m.panes) != 2 {
		t.Fatalf("aggregated %d panes, want 2 (one per Host)", len(m.panes))
	}
	if r0, r1 := m.panes[0].Ref(), m.panes[1].Ref(); r0 == r1 {
		t.Fatalf("panes from different Hosts collided on identity: both %s", r0)
	}
	if got := m.panes[0].Ref().String(); got != "alpha:%0" {
		t.Errorf("first PaneRef = %q, want alpha:%%0", got)
	}

	// Each watch went to its owning Host, exactly once.
	if got := alpha.watchedPanes(); len(got) != 1 || got[0] != "%0" {
		t.Errorf("alpha watched = %v, want [%%0]", got)
	}
	if got := beta.watchedPanes(); len(got) != 1 || got[0] != "%0" {
		t.Errorf("beta watched = %v, want [%%0]", got)
	}

	// Make alpha's pane live, focus it, and type: the input reaches alpha's
	// Server only, even though beta owns a pane with the same bare id.
	alphaRef := herd.PaneRef{Host: "alpha", ID: "%0"}
	m, _ = feed(t, m, paneUpdateMsg{ref: alphaRef, stream: currentStream(m, alphaRef), update: resize(10, 3)})
	m = enterFocus(t, m)
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'z', Text: "z"})
	waitKeys(t, alpha.sink, "z")
	if got := beta.sink.received(); len(got) != 0 {
		t.Errorf("input leaked to beta's Server: %q", got)
	}
}
