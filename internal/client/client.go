// Package client implements the Client side of the shevet.v1 API: dialing a
// Server's unix socket and exposing typed calls to the rest of the Client
// (TUI, CLI).
//
// In this slice the socket is dialed directly on the local machine. The SSH
// transport slice (issue #10) adds reaching the same socket on a remote
// Host; transport selection belongs in this package — the CLI should keep
// parsing targets, not dialing them.
package client

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
	"github.com/irodion/shevet/internal/wire"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// Client is a connection to one Server. Construct with Dial; always Close.
type Client struct {
	conn *grpc.ClientConn
	herd shevetv1.HerdServiceClient
}

// Dial connects to the Server listening on the given unix socket path.
//
// The transport is intentionally unauthenticated: the socket is private to
// the user and reached over SSH, which is the sole security boundary
// (docs/adr/0003-grpc-over-ssh-tunnel.md).
func Dial(socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("socket path must not be empty")
	}
	conn, err := grpc.NewClient(
		"unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial server socket %s: %w", socketPath, err)
	}
	return &Client{
		conn: conn,
		herd: shevetv1.NewHerdServiceClient(conn),
	}, nil
}

// ListPanes returns every Pane in the Server's Herd.
func (c *Client) ListPanes(ctx context.Context) ([]herd.Pane, error) {
	resp, err := c.herd.ListPanes(ctx, &shevetv1.ListPanesRequest{})
	if err != nil {
		return nil, fmt.Errorf("list panes: %w", err)
	}
	panes := make([]herd.Pane, 0, len(resp.GetPanes()))
	for _, p := range resp.GetPanes() {
		panes = append(panes, wire.PaneFromProto(p))
	}
	return panes, nil
}

// PaneUpdate is one event from a Pane's render stream. Exactly one of the
// field groups is meaningful per update: a resize, a damage batch (cells
// plus cursor — cells may be empty for cursor-only movement), or exited.
type PaneUpdate struct {
	// Resized, when non-nil, is the Pane's new size; the receiver's grid
	// resets to default cells.
	Resized *grid.Size

	// Damage, when non-nil, is a batch of cell patches; Cursor is the
	// cursor state after applying it.
	Damage []grid.CellPatch
	Cursor grid.Cursor

	// Exited reports the Pane left the Herd; the stream ends after it.
	Exited bool
}

// PaneView follows a Pane's render stream: it folds PaneUpdates into a
// local grid, realizing the protocol contract in one place — a resize
// resets content, damage re-establishes it, exited is terminal. The TUI's
// Pane widget and the test clients all view a Pane through this type.
type PaneView struct {
	Grid   *grid.Grid
	Cursor grid.Cursor
	Exited bool
}

// NewPaneView returns an empty view, ready for the stream's initial resize.
func NewPaneView() *PaneView {
	return &PaneView{Grid: grid.New(0, 0)}
}

// Apply folds one stream update into the view.
func (v *PaneView) Apply(u PaneUpdate) {
	switch {
	case u.Exited:
		v.Exited = true
	case u.Resized != nil:
		v.Grid = grid.New(u.Resized.W, u.Resized.H)
	case u.Damage != nil:
		for _, p := range u.Damage {
			v.Grid.Apply(p)
		}
		v.Cursor = u.Cursor
	}
}

// PaneWatch is a live render stream for one Pane. Receive with Recv until
// an error or an Exited update; cancel the WatchPane context to stop.
type PaneWatch struct {
	stream grpc.ServerStreamingClient[shevetv1.PaneUpdate]
}

// WatchPane subscribes to a Pane's render stream. The first updates always
// establish full current state (resize, then damage covering every
// non-default cell); afterwards damage carries only changed cells.
func (c *Client) WatchPane(ctx context.Context, paneID string) (*PaneWatch, error) {
	stream, err := c.herd.WatchPane(ctx, &shevetv1.WatchPaneRequest{PaneId: paneID})
	if err != nil {
		return nil, fmt.Errorf("watch pane %s: %w", paneID, err)
	}
	return &PaneWatch{stream: stream}, nil
}

// Recv returns the next update, blocking until one arrives. It returns
// io.EOF when the Server ends the stream.
func (w *PaneWatch) Recv() (PaneUpdate, error) {
	msg, err := w.stream.Recv()
	if err != nil {
		return PaneUpdate{}, err //nolint:wrapcheck // io.EOF must reach callers unwrapped
	}

	switch u := msg.GetUpdate().(type) {
	case *shevetv1.PaneUpdate_Resized:
		return PaneUpdate{Resized: &grid.Size{W: int(u.Resized.GetWidth()), H: int(u.Resized.GetHeight())}}, nil
	case *shevetv1.PaneUpdate_Damage:
		damage, cursor := wire.DamageFromProto(u.Damage)
		return PaneUpdate{Damage: damage, Cursor: cursor}, nil
	case *shevetv1.PaneUpdate_Exited:
		return PaneUpdate{Exited: true}, nil
	default:
		return PaneUpdate{}, fmt.Errorf("unknown pane update %T", u)
	}
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
