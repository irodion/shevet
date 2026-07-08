package server

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/irodion/shevet/internal/inject"
	"github.com/irodion/shevet/internal/wire"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// herdService adapts the Registry, the pane hub, and the tmux injector to the
// shevet.v1 HerdService API. It owns no terminal state of its own: the
// Registry and the pipelines are the render-side sources of truth, and the
// injector is the tmux command seam — the service is translation and routing.
type herdService struct {
	shevetv1.UnimplementedHerdServiceServer

	registry *Registry
	hub      *paneHub

	// injector delivers Control Input to tmux. Nil when the Server has no
	// tmux attachment (an empty Herd), in which case there are no Panes to
	// inject into and SendInput never reaches it.
	injector inject.Commander

	// serveCtx ends when the Server begins shutting down. SendInput is a
	// long-lived client stream with no server-side terminal signal of its
	// own, so it watches serveCtx to return promptly and let GracefulStop
	// complete rather than blocking on a Client that never hangs up.
	serveCtx context.Context
}

func newHerdService(serveCtx context.Context, registry *Registry, hub *paneHub, injector inject.Commander) *herdService {
	return &herdService{
		registry: registry,
		hub:      hub,
		injector: injector,
		serveCtx: serveCtx,
	}
}

func (s *herdService) ListPanes(ctx context.Context, req *shevetv1.ListPanesRequest) (*shevetv1.ListPanesResponse, error) {
	panes := s.registry.ListPanes()

	resp := &shevetv1.ListPanesResponse{
		Panes: make([]*shevetv1.Pane, 0, len(panes)),
	}
	for _, p := range panes {
		resp.Panes = append(resp.Panes, wire.PaneToProto(p))
	}
	return resp, nil
}

// WatchPane subscribes the caller to a Pane's render pipeline and relays
// updates until the Pane exits, the Client hangs up, or the Server stops.
func (s *herdService) WatchPane(req *shevetv1.WatchPaneRequest, stream shevetv1.HerdService_WatchPaneServer) error {
	pipe := s.hub.pipe(req.GetPaneId())
	if pipe == nil {
		return status.Errorf(codes.NotFound, "no pane %q in the Herd", req.GetPaneId())
	}

	sub := pipe.subscribe()

	// relay forwards one queued update, reporting whether the stream is
	// over. A closed channel ends the stream — with the protocol's
	// terminal PaneExited first when the Pane left the Herd (the exited
	// flag is set before the close, so reading it after the drain is
	// safe, and unlike a queued update it can never be dropped).
	relay := func(u renderUpdate, ok bool) (stop bool, err error) {
		if !ok {
			if sub.exited {
				return true, sendExited(stream)
			}
			return true, nil // Server shutting down
		}
		if err := sendUpdate(stream, u); err != nil {
			return true, err
		}
		return false, nil
	}
	for {
		select {
		case u, ok := <-sub.ch:
			stop, err := relay(u, ok)
			if err != nil {
				pipe.unsubscribe(sub)
				return err
			}
			if stop {
				return nil
			}
		case <-stream.Context().Done():
			pipe.unsubscribe(sub)
			return status.FromContextError(stream.Context().Err()).Err()
		case <-pipe.done:
			// The pipeline ended; relay anything it queued first. This
			// also covers a subscribe that raced pipeline close and was
			// never registered.
			for {
				select {
				case u, ok := <-sub.ch:
					if stop, err := relay(u, ok); err != nil || stop {
						return err
					}
				default:
					return nil
				}
			}
		}
	}
}

// SendInput drains the Control Input stream, injecting each event's bytes
// into its target Pane, and returns a summary when the Client half-closes.
//
// Input is fire-and-forward: there is no per-event ack, only the closing
// summary, so a keystroke's latency is one injection, not a round-trip.
// Delivery is best-effort about the Pane's existence — bytes aimed at a Pane
// that has left the Herd are dropped, not fatal, since a Pane can exit while
// the Client is mid-keystroke — but injection failures against a live Pane
// end the stream, because they mean the tmux seam itself is broken.
func (s *herdService) SendInput(stream shevetv1.HerdService_SendInputServer) error {
	events := newInputReceiver(stream)
	defer events.stop()

	// Injection runs under a context that ends when the stream ends *or* the
	// Server shuts down. stream.Context() alone isn't enough: GracefulStop
	// waits on handlers without canceling their contexts, so a tmux command
	// blocked inside inject.Keys would hold shutdown until the Client hangs
	// up. Tying it to serveCtx lets shutdown cancel a wedged injection.
	injectCtx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	defer context.AfterFunc(s.serveCtx, cancel)()

	var summary shevetv1.SendInputSummary
	for {
		select {
		case <-s.serveCtx.Done():
			// Server shutdown: end cleanly so GracefulStop proceeds. The
			// Client observes the stream close and can redial.
			return nil
		case r := <-events.ch:
			if errors.Is(r.err, io.EOF) {
				return stream.SendAndClose(&summary) //nolint:wrapcheck // terminal send; gRPC status is the wire truth
			}
			if r.err != nil {
				return r.err //nolint:wrapcheck // already a gRPC-transport error
			}
			if err := s.injectEvent(injectCtx, r.ev, &summary); err != nil {
				return err
			}
		}
	}
}

// injectEvent routes one input event to its Pane, tallying what it delivered
// into summary. An event with no Pane in the Herd, or of a kind this Server
// doesn't handle yet, is dropped and left uncounted — a benign race, not an
// error. An injection failure against a live Pane is returned as a gRPC error
// that ends the stream.
func (s *herdService) injectEvent(ctx context.Context, ev *shevetv1.InputEvent, summary *shevetv1.SendInputSummary) error {
	keys := ev.GetKeys()
	if keys == nil {
		// A future event kind (focus, resize, paste, mouse). Ignore it
		// forward-compatibly.
		return nil
	}
	paneID, data := keys.GetPaneId(), keys.GetData()

	// The Pane must be in the Herd. Resolving it here also means input can
	// never reach a tmux pane the Server isn't tracking.
	if s.hub.get(paneID) == nil {
		return nil
	}
	if s.injector == nil {
		// Unreachable in practice — a Pane in the hub implies a tmux
		// attachment — but stated so the invariant is explicit, not assumed.
		return status.Error(codes.Unavailable, "server has no tmux attachment to inject input")
	}
	if err := inject.Keys(ctx, s.injector, paneID, data); err != nil {
		return status.Errorf(codes.Unavailable, "inject into pane %s: %v", paneID, err)
	}
	summary.Events++
	summary.Bytes += uint64(len(data))
	return nil
}

// inputResult is one delivery from the Control Input stream.
type inputResult struct {
	ev  *shevetv1.InputEvent
	err error
}

// inputReceiver pumps stream.Recv on its own goroutine so SendInput can also
// select on server shutdown. gRPC's Recv cannot be canceled directly, so the
// goroutine outlives a shutdown-initiated return by at most one Recv — once
// the handler returns, gRPC closes the stream and the pending Recv unblocks.
type inputReceiver struct {
	ch   chan inputResult
	done chan struct{}
}

func newInputReceiver(stream shevetv1.HerdService_SendInputServer) *inputReceiver {
	r := &inputReceiver{
		ch:   make(chan inputResult),
		done: make(chan struct{}),
	}
	go func() {
		for {
			ev, err := stream.Recv()
			select {
			case r.ch <- inputResult{ev: ev, err: err}:
			case <-r.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

// stop releases the receiver goroutine when it is parked trying to deliver.
func (r *inputReceiver) stop() { close(r.done) }

// sendUpdate translates one renderUpdate into its wire messages: a resize,
// then damage — the order receivers rely on.
func sendUpdate(stream shevetv1.HerdService_WatchPaneServer, u renderUpdate) error {
	if u.resized != nil {
		msg := &shevetv1.PaneUpdate{Update: &shevetv1.PaneUpdate_Resized{
			Resized: &shevetv1.PaneResized{Width: uint32(u.resized.W), Height: uint32(u.resized.H)},
		}}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	if u.damage != nil {
		msg := &shevetv1.PaneUpdate{Update: &shevetv1.PaneUpdate_Damage{
			Damage: wire.DamageToProto(u.damage, u.cursor),
		}}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

// sendExited sends the stream's terminal PaneExited message.
func sendExited(stream shevetv1.HerdService_WatchPaneServer) error {
	return stream.Send(&shevetv1.PaneUpdate{
		Update: &shevetv1.PaneUpdate_Exited{Exited: &shevetv1.PaneExited{}},
	})
}
