package agent

// Tests for ADR-W003 W1: per-operation signing (anti-MITM). Real gateway +
// real agent over localhost mTLS, same style as integration_test.go.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// memHandler is a minimal slog.Handler that records emitted records so
// tests can assert a Warn was logged, without depending on log format.
type memHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func newMemLogger() (*slog.Logger, *memHandler) {
	h := &memHandler{}
	return slog.New(h), h
}

func (h *memHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *memHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *memHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *memHandler) WithGroup(string) slog.Handler      { return h }

func (h *memHandler) count(level slog.Level, substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- (a) signed op verifies

func TestSignedOpVerifiesAndExecutes(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	managed := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.DataDir = dataDir
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Join must have pinned the gateway's op-signing public key.
	pinned, err := os.ReadFile(filepath.Join(dataDir, "op-signing.pub"))
	if err != nil {
		t.Fatalf("op-signing.pub not written: %v", err)
	}
	if string(pinned) != string(h.gw.OpSigningPublicKey()) {
		t.Fatalf("pinned key does not match gateway's signing key")
	}

	target := filepath.Join(managed, "signed.txt")
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: target, Content: []byte("hello")},
	}})
	if res.GetError() != nil {
		t.Fatalf("signed op rejected: %+v", res)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "hello" {
		t.Fatalf("signed op did not execute: content=%q err=%v", got, err)
	}
}

// ---------------------------------------------------------------- (b) tampering is rejected, no side effect

func TestTamperedOpsRejectedWithNoSideEffect(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	managed := t.TempDir()
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.ManagedPrefixes = []string{managed} })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	writeOp := func(name, content string) *wsv1.Operation {
		return &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
			WriteManagedFile: &wsv1.WriteManagedFile{Path: filepath.Join(managed, name), Content: []byte(content)},
		}}
	}

	dispatchRaw := func(t *testing.T, msg *wsv1.KeystoneMessage) *wsv1.OpResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := h.gw.DispatchRaw(ctx, "node-a", msg)
		if err != nil {
			t.Fatalf("dispatch raw: %v", err)
		}
		return res
	}

	assertVerificationRejected := func(t *testing.T, path string, res *wsv1.OpResult) {
		t.Helper()
		if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_VERIFICATION {
			t.Fatalf("want VERIFICATION, got %+v", res)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("op had a side effect despite failing verification: %v", err)
		}
	}

	t.Run("tampered op_id", func(t *testing.T) {
		op := writeOp("tampered-op-id.txt", "should-not-write")
		realID := "op-real-id"
		sig, err := h.gw.SignOp(realID, op)
		if err != nil {
			t.Fatal(err)
		}
		// Signature was computed for realID; send it under a different op_id.
		res := dispatchRaw(t, &wsv1.KeystoneMessage{OpId: "op-tampered-id", Op: op, OpSignature: sig})
		assertVerificationRejected(t, filepath.Join(managed, "tampered-op-id.txt"), res)
	})

	t.Run("tampered op bytes", func(t *testing.T) {
		opID := "op-tampered-bytes"
		signedOp := writeOp("tampered-bytes-signed.txt", "signed-content")
		sig, err := h.gw.SignOp(opID, signedOp)
		if err != nil {
			t.Fatal(err)
		}
		// Send a different operation under the same op_id + signature.
		sentOp := writeOp("tampered-bytes-actual.txt", "attacker-content")
		res := dispatchRaw(t, &wsv1.KeystoneMessage{OpId: opID, Op: sentOp, OpSignature: sig})
		assertVerificationRejected(t, filepath.Join(managed, "tampered-bytes-actual.txt"), res)
		if _, err := os.Stat(filepath.Join(managed, "tampered-bytes-signed.txt")); !os.IsNotExist(err) {
			t.Fatal("signed-but-unsent op somehow got written")
		}
	})

	t.Run("missing signature", func(t *testing.T) {
		op := writeOp("missing-sig.txt", "should-not-write")
		res := dispatchRaw(t, &wsv1.KeystoneMessage{OpId: "op-missing-sig", Op: op, OpSignature: nil})
		assertVerificationRejected(t, filepath.Join(managed, "missing-sig.txt"), res)
	})
}

// ---------------------------------------------------------------- (c) unpinned identity accepts unsigned + warns

func TestUnpinnedIdentityAcceptsUnsignedAndWarns(t *testing.T) {
	h := startGateway(t, nil, "")
	managed := t.TempDir()
	dataDir := t.TempDir()

	// Simulate a pre-W1 identity: certs issued directly from the gateway's
	// CA (bypassing Join, which always pins a key on this gateway), and no
	// op-signing.pub written to DataDir.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-legacy"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	certPEM, err := h.gw.CA().IssueFromCSR(csrPEM, "node-legacy")
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dataDir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "node.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "keystone-ca.crt"), h.gw.CA().PEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately no op-signing.pub.

	logger, mh := newMemLogger()
	cfg := Config{
		NodeID:          "node-legacy",
		KeystoneAddr:    h.addr,
		DataDir:         dataDir,
		ManagedPrefixes: []string{managed},
		DockerEndpoint:  "http://127.0.0.1:1",
		BinaryPath:      filepath.Join(t.TempDir(), "wardstoned"),
		Version:         "test",
		BackoffBase:     50 * time.Millisecond,
		BackoffCap:      500 * time.Millisecond,
		Logger:          logger,
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)

	if !h.gw.WaitConnected("node-legacy", 10*time.Second) {
		t.Fatal("legacy agent never connected")
	}

	// Ops still execute even though no key is pinned.
	target := filepath.Join(managed, "legacy.txt")
	res := dispatch(t, h, "node-legacy", &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: target, Content: []byte("legacy-ok")},
	}})
	if res.GetError() != nil {
		t.Fatalf("unsigned op on unpinned identity should still execute: %+v", res)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "legacy-ok" {
		t.Fatalf("unsigned op did not execute: content=%q err=%v", got, err)
	}

	if n := mh.count(slog.LevelWarn, "no pinned op-signing key"); n != 1 {
		t.Fatalf("want exactly 1 warn about unpinned key for the session, got %d", n)
	}
}

// ---------------------------------------------------------------- (d) in-session replay rejected, executed once

// TestReplayedOpRejectedAndExecutedOnce sends the SAME validly-signed
// KeystoneMessage twice (as a transport MITM re-injecting a captured op
// would): the first dispatch executes; the identical second is refused with
// FORBIDDEN and never re-executes. Signature verification alone cannot stop
// this — the replayed bytes carry a genuine signature — so it exercises the
// per-session replay guard (ADR-W003 W1).
func TestReplayedOpRejectedAndExecutedOnce(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	managed := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.DataDir = dataDir
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	target := filepath.Join(managed, "replay.txt")
	op := &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: target, Content: []byte("once")},
	}}
	// A fixed op_id + its valid signature: exactly the bytes a MITM captures.
	opID := "op-fixed-replay"
	sig, err := h.gw.SignOp(opID, op)
	if err != nil {
		t.Fatal(err)
	}
	msg := &wsv1.KeystoneMessage{OpId: opID, Op: op, OpSignature: sig}

	send := func() *wsv1.OpResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := h.gw.DispatchRaw(ctx, "node-a", msg)
		if err != nil {
			t.Fatalf("dispatch raw: %v", err)
		}
		return res
	}

	// First delivery: legitimate, executes.
	if res := send(); res.GetError() != nil {
		t.Fatalf("first (legitimate) op rejected: %+v", res)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "once" {
		t.Fatalf("first op did not execute: content=%q err=%v", got, err)
	}

	// Remove the file so any re-execution of the replay is observable.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	// Second, identical delivery: a replay. Must be refused, not re-executed.
	res := send()
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("want FORBIDDEN for replayed op, got %+v", res)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("replayed op re-executed despite rejection: %s exists again (%v)", target, err)
	}
}
