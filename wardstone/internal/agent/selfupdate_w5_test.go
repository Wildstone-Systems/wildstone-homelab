package agent

// ADR-W003 W5: re-exec ordering, the update-state.json health gate
// (confirm-on-session-establish / revert-on-expired-deadline), and the
// 64 MiB message-size headroom for inline SelfUpdate delivery.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestSelfUpdateStateLifecycle covers the full happy loop without a real
// re-exec: dispatch a self-update, observe execSelf fire only after the
// promoted OpResult has been sent, confirm update-state.json was written
// with the pending version and a future deadline, confirm a startup check
// against a not-yet-expired deadline leaves it untouched, and finally
// confirm that the next successful session establishment (simulating the
// newly-promoted process starting up and dialing Keystone) deletes it.
func TestSelfUpdateStateLifecycle(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "wardstoned")
	if err := os.WriteFile(binPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var dataDir string
	execCalled := make(chan struct{}, 1)
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.BinaryPath = binPath
		c.UpdatePublicKey = pub
		c.ExecSelf = func() { execCalled <- struct{}{} }
		dataDir = c.DataDir
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	newBinary := []byte("new-binary-v2")
	sum := sha256.Sum256(newBinary)
	op := &wsv1.SelfUpdate{
		Binary:    newBinary,
		Signature: ed25519.Sign(priv, sum[:]),
		Version:   "v2",
		Sha256Hex: hex.EncodeToString(sum[:]),
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{SelfUpdate: op}})
	su := res.GetSelfUpdate()
	if su == nil || !su.GetPromoted() {
		t.Fatalf("self-update: %+v", res)
	}

	// The gateway's Dispatch only unblocks once it received the OpResult;
	// execSelf runs in the same agent-side goroutine right after the send
	// that produced it, so this should already be signaled or arrive
	// immediately — but assert with a generous timeout rather than assuming
	// zero latency between "gateway received it" and "agent's own send call
	// returned".
	select {
	case <-execCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("execSelf was not called after the promoted OpResult was sent")
	}

	statePath := filepath.Join(dataDir, updateStateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("update-state.json missing: %v", err)
	}
	var st updateState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("update-state.json: %v", err)
	}
	if st.Pending != "v2" {
		t.Fatalf("pending version = %q, want v2", st.Pending)
	}
	if !st.Deadline.After(time.Now()) {
		t.Fatalf("deadline already passed: %v", st.Deadline)
	}

	// Startup check against a not-yet-expired deadline: state file is left
	// alone, no revert, no re-exec.
	if err := CheckUpdateState(Config{BinaryPath: binPath, DataDir: dataDir}, nil); err != nil {
		t.Fatalf("CheckUpdateState: %v", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file disturbed by a not-yet-expired startup check: %v", err)
	}

	// Simulate the newly-promoted process starting up: a fresh Agent using
	// the same identity/DataDir establishes a session with Keystone. That
	// first successful session establishment must confirm the update —
	// delete the state file and log "self-update confirmed".
	newAgentCfg := Config{
		NodeID:          "node-a",
		KeystoneAddr:    h.addr,
		DataDir:         dataDir,
		ManagedPrefixes: []string{t.TempDir()},
		DockerEndpoint:  "http://127.0.0.1:1",
		BinaryPath:      binPath,
		Version:         "test",
		BackoffBase:     50 * time.Millisecond,
		BackoffCap:      500 * time.Millisecond,
	}
	a2, err := New(newAgentCfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a2.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			return // confirmed
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("update-state.json was not deleted after the confirming session was established")
}

// TestSelfUpdateAutoRevert covers the startup half of the health gate when
// the promoted binary never confirmed: an update-state.json with a PAST
// deadline must cause BinaryPath-prev to be restored over BinaryPath, the
// state file deleted, and execSelf invoked (to re-exec into the restored
// binary).
func TestSelfUpdateAutoRevert(t *testing.T) {
	dataDir := t.TempDir()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "wardstoned")
	if err := os.WriteFile(binPath, []byte("broken-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath+"-prev", []byte("good-v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	st := updateState{Pending: "v2", Deadline: time.Now().Add(-1 * time.Minute)}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, updateStateFileName)
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	execCalled := make(chan struct{}, 1)
	cfg := Config{
		BinaryPath: binPath,
		DataDir:    dataDir,
		ExecSelf:   func() { execCalled <- struct{}{} },
	}
	if err := CheckUpdateState(cfg, nil); err != nil {
		t.Fatalf("CheckUpdateState: %v", err)
	}

	select {
	case <-execCalled:
	default:
		t.Fatal("execSelf was not called on revert")
	}

	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "good-v1" {
		t.Fatalf("binary not restored: got %q, want %q", got, "good-v1")
	}
	if _, err := os.Stat(binPath + "-prev"); !os.IsNotExist(err) {
		t.Fatal("-prev generation still present after being renamed over the binary")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("update-state.json not removed after revert")
	}
}

// TestSelfUpdateOversizedMessageRejectedCleanly asserts the 64 MiB message
// size headroom (ADR-W003 W5): a self-update whose payload pushes the wire
// message over the cap fails the Dispatch call itself — cleanly, not by
// killing the stream — and the agent's session survives to serve a
// subsequent op. A payload comfortably under the cap still goes through the
// real verify+stage+promote path.
func TestSelfUpdateOversizedMessageRejectedCleanly(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "wardstoned")
	if err := os.WriteFile(binPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.BinaryPath = binPath
		c.UpdatePublicKey = pub
		c.ExecSelf = func() {}
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// ~70 MiB: exceeds the 64 MiB cap on both the gateway's send and the
	// agent's receive.
	oversized := bytes.Repeat([]byte{0xAB}, 70<<20)
	oversizedSum := sha256.Sum256(oversized)
	bigOp := &wsv1.SelfUpdate{
		Binary:    oversized,
		Signature: ed25519.Sign(priv, oversizedSum[:]),
		Version:   "too-big",
		Sha256Hex: hex.EncodeToString(oversizedSum[:]),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := h.gw.Dispatch(ctx, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{SelfUpdate: bigOp}}); err == nil {
		t.Fatal("expected the oversized SelfUpdate dispatch to fail")
	}

	// The session must have survived cleanly: a subsequent Health op still
	// succeeds on the same connection.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if hr := res.GetHealth(); hr == nil || !hr.GetOk() {
		t.Fatalf("health after oversized dispatch: %+v", res)
	}

	// ~30 MiB: comfortably under the cap; real verify+stage+promote path.
	okBinary := bytes.Repeat([]byte{0xCD}, 30<<20)
	okSum := sha256.Sum256(okBinary)
	okOp := &wsv1.SelfUpdate{
		Binary:    okBinary,
		Signature: ed25519.Sign(priv, okSum[:]),
		Version:   "v-ok",
		Sha256Hex: hex.EncodeToString(okSum[:]),
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{SelfUpdate: okOp}})
	su := res.GetSelfUpdate()
	if su == nil || !su.GetPromoted() {
		t.Fatalf("30 MiB self-update: %+v", res)
	}
}
