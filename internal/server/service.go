package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/irodion/shevet/internal/wire"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// herdService adapts the Registry and the pane hub to the shevet.v1
// HerdService API. It holds no state of its own: the Registry and the
// pipelines are the sources of truth, the service is pure translation.
type herdService struct {
	shevetv1.UnimplementedHerdServiceServer

	registry *Registry
	hub      *paneHub
}

func newHerdService(registry *Registry, hub *paneHub) *herdService {
	return &herdService{registry: registry, hub: hub}
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

	// relay forwards one queued update, reporting whether the stream is
	// over (channel closed, send failure, or the terminal exited update).
	relay := func(u renderUpdate, ok bool) (stop bool, err error) {
		if !ok {
			return true, nil // pane closed or Server shutting down
		}
		if err := sendUpdate(stream, u); err != nil {
			return true, err
		}
		return u.exited, nil
	}

	sub := pipe.subscribe()
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

// sendUpdate translates one renderUpdate into its wire messages: a resize,
// then damage, then exited — the order receivers rely on.
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
	if u.exited {
		msg := &shevetv1.PaneUpdate{Update: &shevetv1.PaneUpdate_Exited{Exited: &shevetv1.PaneExited{}}}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return nil
}
