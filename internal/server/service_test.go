package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/irodion/shevet/internal/testutil"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// recordingCommander captures the send-keys commands the injector issues and
// can be told to fail, standing in for a real tmux control client.
type recordingCommander struct {
	calls [][]string
	err   error
}

func (r *recordingCommander) Command(_ context.Context, args ...string) ([]string, error) {
	r.calls = append(r.calls, args)
	return nil, r.err
}

// scriptedInput is one item the fake stream hands to the handler: an event or
// a terminal error (io.EOF for a clean half-close).
type scriptedInput struct {
	ev  *shevetv1.InputEvent
	err error
}

// fakeInputStream is a HerdService_SendInputServer driven by a script. Recv
// serves the script and then blocks until the script channel closes, so a
// test can hold the stream open to exercise shutdown mid-stream.
type fakeInputStream struct {
	grpc.ClientStreamingServer[shevetv1.InputEvent, shevetv1.SendInputSummary]

	ctx     context.Context
	in      chan scriptedInput
	summary *shevetv1.SendInputSummary
}

func newFakeInputStream(ctx context.Context) *fakeInputStream {
	return &fakeInputStream{ctx: ctx, in: make(chan scriptedInput)}
}

func (f *fakeInputStream) Context() context.Context { return f.ctx }

func (f *fakeInputStream) Recv() (*shevetv1.InputEvent, error) {
	item, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return item.ev, item.err
}

func (f *fakeInputStream) SendAndClose(s *shevetv1.SendInputSummary) error {
	f.summary = s
	return nil
}

func keyEvent(paneID, data string) *shevetv1.InputEvent {
	return &shevetv1.InputEvent{Event: &shevetv1.InputEvent_Keys{
		Keys: &shevetv1.KeyBytes{PaneId: paneID, Data: []byte(data)},
	}}
}

func resizeEvent(paneID string, w, h uint32) *shevetv1.InputEvent {
	return &shevetv1.InputEvent{Event: &shevetv1.InputEvent_Resize{
		Resize: &shevetv1.ResizeRequest{PaneId: paneID, Width: w, Height: h},
	}}
}

// runSendInput runs the handler against a fresh fake stream, returning the
// stream (for its recorded summary) and a channel carrying the handler's
// return value.
func runSendInput(svc *herdService, ctx context.Context) (*fakeInputStream, <-chan error) {
	stream := newFakeInputStream(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.SendInput(stream) }()
	return stream, done
}

func waitErr(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(testutil.WaitTimeout):
		t.Fatal("SendInput did not return")
		return nil
	}
}

func TestSendInput_InjectsAndSummarizes(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	stream.in <- scriptedInput{ev: keyEvent("%1", "hi")}
	stream.in <- scriptedInput{ev: keyEvent("%1", "\r")}
	close(stream.in) // half-close -> io.EOF

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if stream.summary.GetEvents() != 2 || stream.summary.GetBytes() != 3 {
		t.Errorf("summary = %+v, want events=2 bytes=3", stream.summary)
	}
	// "hi" -> one literal; "\r" -> one hex control. Three send-keys total.
	if len(cmd.calls) != 2 {
		t.Fatalf("issued %d send-keys, want 2:\n%v", len(cmd.calls), cmd.calls)
	}
	if got := strings.Join(cmd.calls[0], " "); got != "send-keys -t %1 -l -- hi" {
		t.Errorf("literal call = %q", got)
	}
	if got := strings.Join(cmd.calls[1], " "); got != "send-keys -t %1 -H 0d" {
		t.Errorf("control call = %q", got)
	}
}

func TestSendInput_DropsInputForPaneNotInHerd(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	stream.in <- scriptedInput{ev: keyEvent("%2", "x")} // %2 is not in the Herd
	close(stream.in)

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Errorf("injected into a pane not in the Herd: %v", cmd.calls)
	}
	if stream.summary.GetEvents() != 0 {
		t.Errorf("summary counted a dropped event: %+v", stream.summary)
	}
}

func TestSendInput_IgnoresUnhandledEventKind(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	stream.in <- scriptedInput{ev: &shevetv1.InputEvent{}} // empty oneof: a future event kind
	close(stream.in)

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if len(cmd.calls) != 0 || stream.summary.GetEvents() != 0 {
		t.Errorf("an unhandled event kind was not ignored: calls=%v summary=%+v", cmd.calls, stream.summary)
	}
}

func TestSendInput_ResizesThroughTmux(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	stream.in <- scriptedInput{ev: resizeEvent("%1", 120, 40)}
	close(stream.in)

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if len(cmd.calls) != 1 {
		t.Fatalf("issued %d tmux commands, want 1:\n%v", len(cmd.calls), cmd.calls)
	}
	if got := strings.Join(cmd.calls[0], " "); got != "resize-window -t %1 -x 120 -y 40" {
		t.Errorf("resize call = %q", got)
	}
	if stream.summary.GetEvents() != 1 || stream.summary.GetBytes() != 0 {
		t.Errorf("summary = %+v, want events=1 bytes=0", stream.summary)
	}
}

func TestSendInput_DropsResizeForPaneNotInHerd(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	stream.in <- scriptedInput{ev: resizeEvent("%2", 80, 24)} // %2 is not in the Herd
	close(stream.in)

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Errorf("resized a pane not in the Herd: %v", cmd.calls)
	}
}

func TestSendInput_DropsImplausibleResize(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	for _, ev := range []*shevetv1.InputEvent{
		resizeEvent("%1", 0, 24),
		resizeEvent("%1", 80, 0),
		resizeEvent("%1", maxResizeDim+1, 24), // past tmux's cap: would bounce as %error
		resizeEvent("%1", 80, maxResizeDim+1),
	} {
		stream.in <- scriptedInput{ev: ev}
	}
	close(stream.in)

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if len(cmd.calls) != 0 {
		t.Errorf("an implausible geometry reached tmux: %v", cmd.calls)
	}
	if stream.summary.GetEvents() != 0 {
		t.Errorf("summary counted dropped resizes: %+v", stream.summary)
	}
}

func TestSendInput_PropagatesInjectionError(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	cmd := &recordingCommander{err: errors.New("tmux control stream ended")}
	svc := newHerdService(context.Background(), NewRegistry(), hub, cmd)

	stream, done := runSendInput(svc, context.Background())
	stream.in <- scriptedInput{ev: keyEvent("%1", "x")}

	err := waitErr(t, done)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("error = %v (code %v), want Unavailable", err, status.Code(err))
	}
	close(stream.in) // release the Recv goroutine
}

func TestSendInput_ReturnsOnServerShutdown(t *testing.T) {
	hub := newPaneHub()
	hub.put("%1", &watchedPane{})
	serveCtx, shutdown := context.WithCancel(context.Background())
	svc := newHerdService(serveCtx, NewRegistry(), hub, &recordingCommander{})

	// The Client keeps the stream open (never sends, never half-closes).
	stream, done := runSendInput(svc, context.Background())

	shutdown() // Server begins shutting down.

	if err := waitErr(t, done); err != nil {
		t.Fatalf("SendInput returned %v on shutdown, want nil", err)
	}
	close(stream.in) // release the Recv goroutine
}
