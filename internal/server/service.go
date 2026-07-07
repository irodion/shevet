package server

import (
	"context"

	"github.com/irodion/shevet/internal/wire"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// herdService adapts the Registry to the shevet.v1 HerdService API.
// It holds no state of its own: the Registry is the source of truth, the
// service is pure translation.
type herdService struct {
	shevetv1.UnimplementedHerdServiceServer

	registry *Registry
}

func newHerdService(registry *Registry) *herdService {
	return &herdService{registry: registry}
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
