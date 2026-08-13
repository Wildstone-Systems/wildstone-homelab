package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"path/filepath"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// rsaCSR builds a valid CSR PEM whose key is an RSA key of the given size.
func rsaCSR(t *testing.T, nodeID string, bits int) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nodeID},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// TestIssueFromCSRRejectsWeakKey proves the CA refuses a CSR carrying a key
// too weak to sign a client cert for, while accepting the node agent's
// ECDSA P-256 key and an adequately-sized RSA key.
func TestIssueFromCSRRejectsWeakKey(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	// 1024-bit RSA — too weak, must be rejected.
	if _, err := ca.IssueFromCSR(rsaCSR(t, "node-a", 1024), "node-a"); err == nil {
		t.Fatal("expected weak (1024-bit RSA) CSR to be rejected")
	}
	// 2048-bit RSA — accepted.
	if _, err := ca.IssueFromCSR(rsaCSR(t, "node-a", 2048), "node-a"); err != nil {
		t.Fatalf("2048-bit RSA CSR should be accepted: %v", err)
	}
	// ECDSA P-256 (the agent's actual key type) — accepted.
	if _, err := ca.IssueFromCSR(testCSR(t, "node-a"), "node-a"); err != nil {
		t.Fatalf("ECDSA P-256 CSR should be accepted: %v", err)
	}
}

// TestCADistinctSerials proves two independently-created CAs do not share a
// serial number (a fixed serial made two data dirs collide on serial 1).
func TestCADistinctSerials(t *testing.T) {
	ca1, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	if ca1.cert.SerialNumber.Cmp(ca2.cert.SerialNumber) == 0 {
		t.Fatalf("two CAs share serial %s — expected distinct random serials", ca1.cert.SerialNumber)
	}
	if ca1.cert.SerialNumber.Sign() == 0 {
		t.Fatal("CA serial is zero")
	}
}

// TestJoinAndRenewAreAudited proves the identity-issuing RPCs write to the
// audit trail: a successful Join, a Join rejected for a bad token, and a
// Join rejected for a bad CSR all leave a record.
func TestJoinIsAudited(t *testing.T) {
	dir := t.TempDir()
	g, err := New(Config{DataDir: dir, JoinTokens: []string{"good-token"}})
	if err != nil {
		t.Fatal(err)
	}

	// Successful join.
	if _, err := g.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-a", JoinToken: "good-token", CsrPem: testCSR(t, "node-a"),
	}); err != nil {
		t.Fatalf("join with good token: %v", err)
	}
	// Bad token.
	if _, err := g.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-b", JoinToken: "nope", CsrPem: testCSR(t, "node-b"),
	}); err == nil {
		t.Fatal("join with bad token should fail")
	}
	// Good token (a fresh one), bad CSR.
	g2, err := New(Config{DataDir: t.TempDir(), JoinTokens: []string{"tok2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g2.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-c", JoinToken: "tok2", CsrPem: []byte("not a csr"),
	}); err == nil {
		t.Fatal("join with bad CSR should fail")
	}

	recs := readAuditRecords(t, filepath.Join(dir, "audit.log"))
	var okJoin, badToken bool
	for _, r := range recs {
		if r.Arm != "Join" {
			t.Fatalf("unexpected audit arm %q", r.Arm)
		}
		switch {
		case r.Node == "node-a" && r.Outcome == "ok":
			okJoin = true
		case r.Node == "node-b" && r.Outcome == "error" && r.ErrorCode == "BAD_TOKEN":
			badToken = true
		}
	}
	if !okJoin {
		t.Error("successful Join was not audited")
	}
	if !badToken {
		t.Error("bad-token Join was not audited")
	}

	badCSR := readAuditRecords(t, filepath.Join(g2.cfg.DataDir, "audit.log"))
	var sawBadCSR bool
	for _, r := range badCSR {
		if r.Arm == "Join" && r.Outcome == "error" && r.ErrorCode == "BAD_CSR" {
			sawBadCSR = true
		}
	}
	if !sawBadCSR {
		t.Error("bad-CSR Join was not audited")
	}
}
