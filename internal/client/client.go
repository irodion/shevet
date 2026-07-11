// Package client implements the Client side of the shevet.v1 API: dialing a
// Server's unix socket and exposing typed calls to the rest of the Client
// (TUI, CLI).
//
// Dial reaches a Server socket on the local machine; DialSSH reaches the same
// socket on a remote Host over SSH (see ssh.go and internal/sshx). Transport
// selection lives here — the CLI parses targets, this package dials them.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/irodion/shevet/internal/grid"
	"github.com/irodion/shevet/internal/herd"
	"github.com/irodion/shevet/internal/wire"
	shevetv1 "github.com/irodion/shevet/proto/shevet/v1"
)

// Client is a connection to one Server. Construct with Dial (local socket) or
// DialSSH (remote Host over SSH); always Close.
type Client struct {
	conn *grpc.ClientConn
	herd shevetv1.HerdServiceClient

	// transport is the SSH connection underlying a DialSSH client, closed
	// after the gRPC connection. Nil for a local Dial.
	transport io.Closer
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

// InputStream is a live Control Input stream to a Server. Forward keystrokes
// with SendKeys; end it with Close, which returns the Server's delivery
// summary. The stream is long-lived: one open stream carries a focused
// session's typing, so a keystroke costs a Send, not an RPC handshake.
type InputStream struct {
	stream grpc.ClientStreamingClient[shevetv1.InputEvent, shevetv1.SendInputSummary]
}

// InputSummary is the Server's tally of a closed Control Input stream: how
// many events it accepted and how many bytes it injected. It is a diagnostic
// acknowledgement, not a per-keystroke ack.
type InputSummary struct {
	Events uint64
	Bytes  uint64
}

// SendInput opens a Control Input stream to the Server. Cancel ctx (or Close
// the Client) to tear it down; the returned stream is not safe for concurrent
// SendKeys.
func (c *Client) SendInput(ctx context.Context) (*InputStream, error) {
	stream, err := c.herd.SendInput(ctx)
	if err != nil {
		return nil, fmt.Errorf("open input stream: %w", err)
	}
	return &InputStream{stream: stream}, nil
}

// SendKeys forwards raw input bytes for a Pane. The Server injects them
// exactly — printable UTF-8 literally, control bytes by hex — so the Client
// is responsible only for encoding keystrokes to terminal bytes, never for
// how tmux delivers them. Empty data is a no-op.
func (s *InputStream) SendKeys(paneID string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	err := s.stream.Send(&shevetv1.InputEvent{Event: &shevetv1.InputEvent_Keys{
		Keys: &shevetv1.KeyBytes{PaneId: paneID, Data: data},
	}})
	if err != nil {
		return fmt.Errorf("send keys to pane %s: %w", paneID, err)
	}
	return nil
}

// SendResize asks the Server to resize a Pane to w×h cells — the
// resize-on-focus contract: a focusing Client sends its viewport size, and on
// unfocus the Pane's prior size. Confirmation arrives on the Pane's render
// stream as a PaneResized, never here. A non-positive size is a programming
// error and is rejected before it can reach the wire.
func (s *InputStream) SendResize(paneID string, w, h int) error {
	if w <= 0 || h <= 0 {
		return fmt.Errorf("resize pane %s: implausible size %dx%d", paneID, w, h)
	}
	err := s.stream.Send(&shevetv1.InputEvent{Event: &shevetv1.InputEvent_Resize{
		Resize: &shevetv1.ResizeRequest{PaneId: paneID, Width: uint32(w), Height: uint32(h)},
	}})
	if err != nil {
		return fmt.Errorf("send resize of pane %s: %w", paneID, err)
	}
	return nil
}

// SendRestore asks the Server to return a Pane to the size it had before
// this stream's first SendResize of it — the unfocus half of resize-on-focus.
// The Server owns the record, so the Client carries no size bookkeeping; a
// restore with no prior resize on this stream is a Server-side no-op.
func (s *InputStream) SendRestore(paneID string) error {
	err := s.stream.Send(&shevetv1.InputEvent{Event: &shevetv1.InputEvent_Restore{
		Restore: &shevetv1.RestoreSize{PaneId: paneID},
	}})
	if err != nil {
		return fmt.Errorf("send restore of pane %s: %w", paneID, err)
	}
	return nil
}

// Close half-closes the stream and returns the Server's summary. After Close
// the stream must not be used again.
func (s *InputStream) Close() (InputSummary, error) {
	sum, err := s.stream.CloseAndRecv()
	if err != nil {
		return InputSummary{}, fmt.Errorf("close input stream: %w", err)
	}
	return InputSummary{Events: sum.GetEvents(), Bytes: sum.GetBytes()}, nil
}

// Close releases the gRPC connection and, for an SSH client, the underlying
// SSH connection beneath it.
func (c *Client) Close() error {
	err := c.conn.Close()
	if c.transport != nil {
		err = errors.Join(err, c.transport.Close())
	}
	return err //nolint:wrapcheck // aggregate teardown error, already contextual
}
