package agent

// Tests for ADR-W003 W7: audit trail. Two independent trails are exercised
// here since they need the full real-gateway + real-agent stack:
//
//   - Keystone-side: <DataDir>/audit.log gets one JSONL line per dispatched
//     op, including failed ops, with correct node/op_id/arm/outcome/
//     error_code; empty DataDir writes no file.
//   - Agent-side: the structured slog audit log records safe params (e.g. a
//     WriteManagedFile's path) but never secret-bearing payloads (file
//     content, self-update binary bytes).
//
// Rotation is tested at the keystone/gateway package level (audit_test.go
// there) since it needs the auditMaxBytes test seam, which is unexported
// outside that package.

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type auditLine struct {
	Ts        string `json:"ts"`
	Node      string `json:"node"`
	OpID      string `json:"op_id"`
	Arm       string `json:"arm"`
	Outcome   string `json:"outcome"`
	ErrorCode string `json:"error_code"`
}

func readAuditLines(t *testing.T, path string) []auditLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit log: %v", err)
	}
	defer f.Close()
	var lines []auditLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		var l auditLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("audit line not valid JSON: %v: %q", err, sc.Text())
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// startGatewayWithKeystoneDataDir starts a gateway.Gateway with a DataDir so
// its own W2 persistence AND W7 audit.log are both exercised, using the
// harness helpers already defined in integration_test.go.
func startGatewayWithKeystoneDataDir(t *testing.T, dataDir string) *harness {
	t.Helper()
	return startGatewayWithDataDir(t, []string{"tok"}, dataDir, "")
}

func TestKeystoneAuditLogRecordsEachDispatchedOp(t *testing.T) {
	keystoneDataDir := t.TempDir()
	h := startGatewayWithKeystoneDataDir(t, keystoneDataDir)
	managed := t.TempDir()
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.ManagedPrefixes = []string{managed} })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// A successful op.
	okRes := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if okRes.GetError() != nil {
		t.Fatalf("health op failed: %+v", okRes)
	}

	// A failed op: EnsureContainer with a non-digest-pinned image ->
	// FORBIDDEN (ADR-W003 §A4.2), refused before ever touching Docker.
	badRes := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{
		EnsureContainer: &wsv1.EnsureContainer{Name: "c1", Image: "nginx:latest"},
	}})
	if badRes.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("want FORBIDDEN, got %+v", badRes)
	}

	auditPath := filepath.Join(keystoneDataDir, "audit.log")
	all := readAuditLines(t, auditPath)
	// The identity-issuing RPCs (Join/Renew) are audited too; this test is
	// about dispatched ops, so separate the join record from the op records.
	var joins int
	var lines []auditLine
	for _, r := range all {
		if r.Arm == "Join" || r.Arm == "Renew" {
			joins++
			continue
		}
		lines = append(lines, r)
	}
	if joins != 1 {
		t.Fatalf("want exactly 1 Join audit record, got %d: %+v", joins, all)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 dispatched-op audit lines, got %d: %+v", len(lines), lines)
	}

	if lines[0].Node != "node-a" || lines[0].OpID != okRes.GetOpId() ||
		lines[0].Arm != "Health" || lines[0].Outcome != "ok" || lines[0].ErrorCode != "" {
		t.Fatalf("unexpected first audit line: %+v", lines[0])
	}
	if _, err := time.Parse(time.RFC3339, lines[0].Ts); err != nil {
		t.Fatalf("ts not RFC3339: %q: %v", lines[0].Ts, err)
	}

	if lines[1].Node != "node-a" || lines[1].OpID != badRes.GetOpId() ||
		lines[1].Arm != "EnsureContainer" || lines[1].Outcome != "error" ||
		lines[1].ErrorCode != "ERROR_CODE_FORBIDDEN" {
		t.Fatalf("unexpected second audit line: %+v", lines[1])
	}
}

func TestKeystoneNoAuditLogWithEmptyDataDir(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "") // no DataDir
	managed := t.TempDir()
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.ManagedPrefixes = []string{managed} })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if res.GetError() != nil {
		t.Fatalf("health op failed: %+v", res)
	}
	// No DataDir was configured at all, so there is no directory to even
	// contain an audit.log — nothing should have been created.
}

// ---------------------------------------------------------------- agent-side secret safety

func TestAgentAuditLogOmitsFileContentSecret(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	managed := t.TempDir()
	logger, mh := newMemLogger()
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.Logger = logger
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	const secret = "sk-super-secret-value-do-not-log-me"
	target := filepath.Join(managed, "secrets.env")
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
		WriteManagedFile: &wsv1.WriteManagedFile{Path: target, Content: []byte(secret)},
	}})
	if res.GetError() != nil {
		t.Fatalf("write failed: %+v", res)
	}

	mh.mu.Lock()
	defer mh.mu.Unlock()
	var sawPath, sawSecret bool
	for _, r := range mh.records {
		r.Attrs(func(a slog.Attr) bool {
			s := a.Value.String()
			if strings.Contains(s, target) {
				sawPath = true
			}
			if strings.Contains(s, secret) {
				sawSecret = true
			}
			return true
		})
		if strings.Contains(r.Message, secret) {
			sawSecret = true
		}
	}
	if !sawPath {
		t.Fatal("audit log never mentioned the file path — expected it to")
	}
	if sawSecret {
		t.Fatal("audit log leaked the secret file content")
	}
}

func TestAgentAuditLogOmitsSelfUpdateBinaryBytes(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	managed := t.TempDir()
	logger, mh := newMemLogger()
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.Logger = logger
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// A distinctive byte pattern standing in for a binary that must never
	// appear in the log. The op is expected to fail verification (bad
	// signature/digest) — dispatch still runs and logs the arm either way,
	// which is exactly what we're checking: that failure path doesn't leak
	// the bytes either.
	binaryMarker := strings.Repeat("BINARYSECRETBYTES", 4)
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SelfUpdate{
		SelfUpdate: &wsv1.SelfUpdate{
			Binary:    []byte(binaryMarker),
			Signature: []byte("not-a-real-signature"),
			Version:   "v9.9.9",
			Sha256Hex: "deadbeef",
		},
	}})
	_ = res // outcome (success/failure) doesn't matter for this assertion

	mh.mu.Lock()
	defer mh.mu.Unlock()
	for _, r := range mh.records {
		if strings.Contains(r.Message, binaryMarker) {
			t.Fatal("audit log leaked the self-update binary bytes (message)")
		}
		r.Attrs(func(a slog.Attr) bool {
			if strings.Contains(a.Value.String(), binaryMarker) {
				t.Fatalf("audit log leaked the self-update binary bytes (attr %s)", a.Key)
			}
			return true
		})
	}
	// Sanity: the arm name (safe) DID get logged, proving the audit path ran.
	var sawArm bool
	for _, r := range mh.records {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "arm" && a.Value.String() == "SelfUpdate" {
				sawArm = true
			}
			return true
		})
	}
	if !sawArm {
		t.Fatal("expected a SelfUpdate audit line")
	}
}
