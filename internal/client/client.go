// Package client implements the Client side of the shevet.v1 API: dialing a
// Server's unix socket and exposing typed calls to the rest of the Client
// (TUI, CLI).
//
// In this slice the socket is dialed directly on the local machine; the SSH
// tunnel slice routes the same dial through a remote Host without changing
// this package's interface.
package client

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/irodion/shevet/internal/herd"
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
		panes = append(panes, herd.Pane{
			ID:    p.GetId(),
			Title: p.GetTitle(),
		})
	}
	return panes, nil
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
