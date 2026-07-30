package agent

// Tests for ADR-W003 W2 (Keystone persistence) that need real agents: a
// Keystone restart from the same DataDir must not orphan already-joined
// agents. Companion unit tests that don't need a live agent (CA/key
// reload, minted-token expiry/persistence, file modes) live in
// keystone/gateway/persist_test.go.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/gateway"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// startGatewayWithDataDir is startGateway's DataDir-aware sibling: the
// gateway persists its CA/op-signing key/tokens to dataDir instead of
// keeping them in memory only, so a later, completely independent
// gateway.New over the same dataDir picks up the same identity.
func startGatewayWithDataDir(t *testing.T, tokens []string, dataDir, addr string) *harness {
	t.Helper()
	gw, err := gateway.New(gateway.Config{
		JoinTokens: tokens,
		Hosts:      []string{"127.0.0.1"},
		DataDir:    dataDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{gw: gw, addr: lis.Addr().String(), done: make(chan error, 1)}
	go func() { h.done <- gw.Serve(lis) }()
	t.Cleanup(gw.Stop)
	return h
}

// TestGatewayRestartWithDataDirKeepsAgentsConnectable is the W2 restart
// test the ADR calls for: kill a Gateway backed by a DataDir, build a
// completely fresh *gateway.Gateway (fresh gateway.New call, not the same
// Go object — mirrors an actual Keystone process restart) from the SAME
// DataDir, serve on the SAME address, and confirm already-joined agents
// reconnect without rejoining (their pinned CA still validates against the
// reloaded CA) and that a dispatched, signed Health op succeeds (which
// requires the reloaded op-signing key to match what agents pinned at
// join, since verification happens agent-side against the pinned pub key).
func TestGatewayRestartWithDataDirKeepsAgentsConnectable(t *testing.T) {
	const agents = 5
	dataDir := t.TempDir()
	tokens := make([]string, agents)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok-%d", i)
	}

	h := startGatewayWithDataDir(t, tokens, dataDir, "")
	addr := h.addr

	for i := 0; i < agents; i++ {
		newTestAgent(t, h, fmt.Sprintf("node-%d", i), tokens[i], nil)
	}
	for i := 0; i < agents; i++ {
		if !h.gw.WaitConnected(fmt.Sprintf("node-%d", i), 15*time.Second) {
			t.Fatalf("node-%d never connected initially", i)
		}
	}

	origSignPub := h.gw.OpSigningPublicKey()
	origCAPEM := string(h.gw.CA().PEM())

	// Kill the whole gateway — every agent loses its stream.
	h.gw.Stop()

	// Rebuild from scratch: a brand new Gateway object, loaded from the
	// same DataDir, exactly what a restarted `keystone` process does.
	gw2, err := gateway.New(gateway.Config{
		Hosts:   []string{"127.0.0.1"},
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("rebuild gateway from same data dir: %v", err)
	}
	if string(gw2.CA().PEM()) != origCAPEM {
		t.Fatal("reloaded gateway's CA differs from the original — agents' pinned CA would go stale")
	}
	if string(gw2.OpSigningPublicKey()) != string(origSignPub) {
		t.Fatal("reloaded gateway's op-signing key differs from the original — agents' pinned key would go stale")
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw2.Stop)
	go gw2.Serve(lis) //nolint:errcheck
	h.gw = gw2

	var wg sync.WaitGroup
	failures := make(chan string, agents)
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			node := fmt.Sprintf("node-%d", n)
			if !gw2.WaitConnected(node, 30*time.Second) {
				failures <- node + ": never reconnected after gateway restart (agent would have had to rejoin if the CA had changed)"
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// A successful, signed Health round trip proves the reloaded
			// op-signing key is the one the agent pinned at join: the
			// agent refuses any op whose signature doesn't verify against
			// its pinned op-signing.pub (ADR-W003 W1).
			res, err := gw2.Dispatch(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
			if err != nil || !res.GetHealth().GetOk() {
				failures <- fmt.Sprintf("%s: post-restart signed health failed: %v %+v", node, err, res)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}
}
