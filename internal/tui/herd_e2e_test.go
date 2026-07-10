package tui

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/irodion/shevet/internal/client"
	"github.com/irodion/shevet/internal/harness"
	"github.com/irodion/shevet/internal/testutil"
)

// waitForPanes polls a Server's Herd until it holds at least min Panes, so the
// aggregation assertion doesn't race the Server's initial reconcile. It drives
// the public client, not the code under test — this is setup, not the subject.
func waitForPanes(t *testing.T, c *client.Client, min int) {
	t.Helper()
	deadline := time.After(testutil.WaitTimeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitTimeout)
		panes, err := c.ListPanes(ctx)
		cancel()
		if err == nil && len(panes) >= min {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("Herd never reached %d panes (had %d, err=%v)", min, len(panes), err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestHerd_TwoLocalServersCoexist is the acceptance e2e for issue #55: two
// real local-socket Servers whose Panes carry the same tmux ids (each sandbox
// has a %0 and a %1) aggregate into one Client model as distinct PaneRefs.
// It proves Client-scoped identity holds over the real gRPC wire — where the
// Pane messages carry only bare, per-Host pane ids (no host field) — not just
// against fakes.
func TestHerd_TwoLocalServersCoexist(t *testing.T) {
	alpha := harness.Start(t)
	beta := harness.Start(t)

	// A scripted agent per Host gives each Herd a second Pane (%1) alongside
	// the sandbox's initial shell (%0); both ids recur across the two Hosts.
	alpha.StartAgent(t, "agent-alpha", "prompt ready > \nawait-line\n")
	beta.StartAgent(t, "agent-beta", "prompt ready > \nawait-line\n")
	waitForPanes(t, alpha.Client, 2)
	waitForPanes(t, beta.Client, 2)

	servers, err := NewServers([]Server{
		{Alias: "alpha", Conn: FromClient(alpha.Client)},
		{Alias: "beta", Conn: FromClient(beta.Client)},
	})
	if err != nil {
		t.Fatalf("NewServers: %v", err)
	}

	m := New(context.Background(), servers)
	m, _ = pump(t, m, m.Init()) // real ListPanes over gRPC to both Servers

	// Every aggregated Pane has a unique PaneRef, even though bare ids repeat
	// across the two Hosts.
	seen := make(map[string]bool, len(m.panes))
	var alphaZero, betaZero bool
	for _, p := range m.panes {
		ref := p.Ref()
		if seen[ref.String()] {
			t.Fatalf("PaneRef %s appeared twice — Hosts collided on identity", ref)
		}
		seen[ref.String()] = true
		switch {
		case ref.Host == "alpha" && ref.ID == "%0":
			alphaZero = true
		case ref.Host == "beta" && ref.ID == "%0":
			betaZero = true
		}
	}
	if !alphaZero || !betaZero {
		t.Fatalf("want a %%0 under both Hosts; got refs %v", slices.Collect(maps.Keys(seen)))
	}
}
