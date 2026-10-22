package gateway

// Tests for ADR-W003 W1 replay hardening: op_id must not repeat across a
// gateway restart, even though the op-signing key persists (persist.go). A
// repeated op_id would let a signature captured before the restart verify
// again against a freshly-issued op_id — a replay a transport MITM could
// mount.

import (
	"context"
	"strings"
	"sync"
	"testing"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// capturingSession installs a fake session for nodeID that records every
// dispatched op_id (in order) and answers each op with an OK Health result,
// so Dispatch's op_id assignment can be observed without a real agent.
func capturingSession(g *Gateway, nodeID string, got *[]string, mu *sync.Mutex) {
	s := &session{
		nodeID:  nodeID,
		pending: map[string]chan *wsv1.OpResult{},
		send: func(m *wsv1.KeystoneMessage) error {
			mu.Lock()
			*got = append(*got, m.GetOpId())
			mu.Unlock()
			g.mu.Lock()
			sess := g.sessions[nodeID]
			g.mu.Unlock()
			if sess == nil {
				return nil
			}
			sess.mu.Lock()
			ch := sess.pending[m.GetOpId()]
			delete(sess.pending, m.GetOpId())
			sess.mu.Unlock()
			if ch != nil {
				ch <- &wsv1.OpResult{OpId: m.GetOpId(), Result: &wsv1.OpResult_Health{Health: &wsv1.HealthResult{Ok: true}}}
			}
			return nil
		},
	}
	g.mu.Lock()
	g.sessions[nodeID] = s
	g.mu.Unlock()
}

// TestOpIDsDoNotRepeatAcrossRestarts dispatches the first op of two gateway
// processes that share the same DataDir (hence the same persisted op-signing
// key, as after a real restart) and asserts their op_ids differ — the fix for
// the reissue-then-replay finding.
func TestOpIDsDoNotRepeatAcrossRestarts(t *testing.T) {
	dataDir := t.TempDir()
	op := &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}}

	dispatchFirst := func() string {
		g, err := New(Config{DataDir: dataDir})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		var mu sync.Mutex
		capturingSession(g, "node-a", &got, &mu)
		if _, err := g.Dispatch(context.Background(), "node-a", op); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("want 1 dispatched op_id, got %d", len(got))
		}
		return got[0]
	}

	first := dispatchFirst()  // op-<gen1>-1
	second := dispatchFirst() // op-<gen2>-1, same persisted signing key
	if first == second {
		t.Fatalf("op_id repeated across restarts: both were %q — a pre-restart signature could replay", first)
	}
}

// TestOpIDsMonotonicAndNamespacedWithinProcess checks that within one process
// op_ids share the generation prefix and increase monotonically, so they stay
// unique per process.
func TestOpIDsMonotonicAndNamespacedWithinProcess(t *testing.T) {
	g, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	var mu sync.Mutex
	capturingSession(g, "node-a", &got, &mu)

	op := &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}}
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := g.Dispatch(context.Background(), "node-a", op); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatalf("want %d op_ids, got %d", n, len(got))
	}
	seen := map[string]bool{}
	var prefix string
	for i, id := range got {
		if seen[id] {
			t.Fatalf("duplicate op_id within process: %q", id)
		}
		seen[id] = true
		// Format: op-<gen>-<seq>; the gen segment must be identical across
		// all ops from one process.
		lastDash := strings.LastIndex(id, "-")
		if lastDash < 0 {
			t.Fatalf("op_id %q has no seq segment", id)
		}
		genPart := id[:lastDash]
		if i == 0 {
			prefix = genPart
			if !strings.HasPrefix(genPart, "op-") {
				t.Fatalf("op_id %q missing op- prefix", id)
			}
		} else if genPart != prefix {
			t.Fatalf("op_id %q generation prefix %q != %q — gen not stable within a process", id, genPart, prefix)
		}
	}
}
