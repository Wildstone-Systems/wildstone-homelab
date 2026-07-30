package gateway

// Unit tests for ADR-W003 W7's rotation behavior, using the auditMaxBytes
// test seam. Basic "one line per dispatched op, correct fields, no file
// with an empty DataDir" coverage lives in wardstone/internal/agent's
// audit_test.go since it needs a real connected agent (same convention as
// persist_test.go's restart test) — these tests instead fake a session
// in-package so they can dispatch many ops cheaply without a real agent.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// installFakeSession registers a session for nodeID whose "agent" answers
// every dispatched op immediately with an OK Health result, without any
// real network connection — enough to drive Dispatch's audit side effect
// repeatedly and cheaply.
func installFakeSession(g *Gateway, nodeID string) {
	s := &session{
		nodeID:  nodeID,
		pending: map[string]chan *wsv1.OpResult{},
		send: func(m *wsv1.KeystoneMessage) error {
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

func readAuditRecords(t *testing.T, path string) []auditRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var recs []auditRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		var r auditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad audit JSON line %q: %v", sc.Text(), err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestAuditLogRotatesAtThreshold(t *testing.T) {
	dataDir := t.TempDir()
	g, err := New(Config{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	installFakeSession(g, "node-a")

	old := auditMaxBytes
	auditMaxBytes = 200 // tiny threshold so a handful of ops crosses it
	t.Cleanup(func() { auditMaxBytes = old })

	op := &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}}
	const n = 20
	for i := 0; i < n; i++ {
		if _, err := g.Dispatch(t.Context(), "node-a", op); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}

	auditPath := filepath.Join(dataDir, "audit.log")
	rotatedPath := auditPath + ".1"

	if _, err := os.Stat(rotatedPath); err != nil {
		t.Fatalf("expected rotated audit.log.1 to exist: %v", err)
	}
	fi, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("audit.log missing after rotation: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("current audit.log unexpectedly empty")
	}

	// Exactly one generation is kept: rotate again and confirm .1 got
	// replaced, not accumulated as .2.
	if _, err := os.Stat(auditPath + ".2"); !os.IsNotExist(err) {
		t.Fatalf("expected no second-generation rotated file, stat err = %v", err)
	}

	// Every record across both files must still be well-formed. Rotation
	// keeps exactly one generation, so records older than that are expected
	// to be gone — this just checks what remains is intact, not that all n
	// survived.
	recs := readAuditRecords(t, rotatedPath)
	recs = append(recs, readAuditRecords(t, auditPath)...)
	if len(recs) == 0 || len(recs) > n {
		t.Fatalf("want 1..%d total audit records across audit.log + audit.log.1, got %d", n, len(recs))
	}
	for _, r := range recs {
		if r.Node != "node-a" || r.Arm != "Health" || r.Outcome != "ok" {
			t.Fatalf("unexpected record: %+v", r)
		}
	}
}

func TestAuditLogKeepsExactlyOneGeneration(t *testing.T) {
	dataDir := t.TempDir()
	g, err := New(Config{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	installFakeSession(g, "node-a")

	old := auditMaxBytes
	auditMaxBytes = 150
	t.Cleanup(func() { auditMaxBytes = old })

	op := &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}}
	// Dispatch enough ops to rotate multiple times over.
	for i := 0; i < 60; i++ {
		if _, err := g.Dispatch(t.Context(), "node-a", op); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}

	if _, err := os.Stat(filepath.Join(dataDir, "audit.log.2")); !os.IsNotExist(err) {
		t.Fatalf("a second rotated generation should never exist, stat err = %v", err)
	}
}
