package agent

// Tests for ADR-W003 W3: CA pinning at join, replacing bare TOFU. Real
// gateway + real agent over localhost mTLS, same style as
// integration_test.go / signing_test.go.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// ---------------------------------------------------------------- (1) correct fingerprint

func TestCAPinningCorrectFingerprintJoinsAndWorks(t *testing.T) {
	h := startGateway(t, []string{"tok-1"}, "")
	managed := t.TempDir()
	newTestAgent(t, h, "node-a", "tok-1", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.CAFingerprint = h.gw.CA().Fingerprint()
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if hr := res.GetHealth(); hr == nil || !hr.GetOk() {
		t.Fatalf("health: %+v", res)
	}
}

// ---------------------------------------------------------------- (2) wrong fingerprint

// TestCAPinningWrongFingerprintFailsBeforeTokenSpent proves the ADR's most
// important W3 property: a wrong --ca-fingerprint fails the join, and the
// join token is NOT consumed — a second agent with the correct fingerprint
// and the SAME token still joins successfully.
func TestCAPinningWrongFingerprintFailsBeforeTokenSpent(t *testing.T) {
	h := startGateway(t, []string{"tok-shared"}, "")
	managed := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")

	cfg := Config{
		NodeID:          "node-a",
		KeystoneAddr:    h.addr,
		DataDir:         dataDir,
		JoinToken:       "tok-shared",
		ManagedPrefixes: []string{managed},
		DockerEndpoint:  "http://127.0.0.1:1",
		BinaryPath:      filepath.Join(t.TempDir(), "wardstoned"),
		Version:         "test",
		CAFingerprint:   strings.Repeat("00", 32), // syntactically valid, definitely wrong
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.join(context.Background()); err == nil {
		t.Fatal("expected join to fail with a wrong CA fingerprint")
	}
	// Nothing should have been persisted.
	for _, p := range []string{"node.crt", "node.key", "keystone-ca.crt"} {
		if _, err := os.Stat(filepath.Join(dataDir, p)); err == nil {
			t.Fatalf("%s should not exist after a failed pinned join", p)
		}
	}

	// The token must still be usable: a second agent, correct fingerprint,
	// same token, joins fine.
	newTestAgent(t, h, "node-b", "tok-shared", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.CAFingerprint = h.gw.CA().Fingerprint()
	})
	if !h.gw.WaitConnected("node-b", 10*time.Second) {
		t.Fatal("second agent never connected — the join token appears to have been spent by the failed pinned join")
	}
}

// ---------------------------------------------------------------- (3) no fingerprint: TOFU

func TestCAPinningUnsetFingerprintTOFUStillWorksAndWarns(t *testing.T) {
	h := startGateway(t, []string{"tok-1"}, "")
	managed := t.TempDir()
	log, mh := newMemLogger()
	newTestAgent(t, h, "node-a", "tok-1", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.Logger = log
		// CAFingerprint deliberately left unset: TOFU.
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if hr := res.GetHealth(); hr == nil || !hr.GetOk() {
		t.Fatalf("health: %+v", res)
	}
	if n := mh.count(slog.LevelWarn, "TOFU join"); n != 1 {
		t.Fatalf("expected exactly one TOFU join warning, got %d", n)
	}
}
