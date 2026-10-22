package gateway

// Tests for ADR-W003 W2: Keystone persistence (CA, op-signing key, join
// tokens). Gateway-restart-with-live-agents lives alongside the existing
// integration tests in wardstone/internal/agent (it needs the agent side);
// everything here is exercised directly against the gateway package without
// standing up a network listener.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// testCSR builds a syntactically valid CSR PEM for nodeID so Join's
// signature check passes.
func testCSR(t *testing.T, nodeID string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
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

// ---------------------------------------------------------------- CA persistence

func TestLoadOrCreateCA_CreatesThenReloadsIdentical(t *testing.T) {
	dir := t.TempDir()

	ca1, loaded, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded {
		t.Fatal("first call should have created, not loaded")
	}

	ca2, loaded, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded {
		t.Fatal("second call should have loaded the persisted CA")
	}
	if string(ca1.PEM()) != string(ca2.PEM()) {
		t.Fatal("reloaded CA certificate differs from the original")
	}

	// A cert issued by the reloaded CA must chain to the original CA's pool
	// — proves the private key, not just the cert, survived.
	certPEM, err := ca2.IssueFromCSR(testCSR(t, "node-a"), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca1.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("cert issued by reloaded CA does not chain to original CA: %v", err)
	}
}

// ---------------------------------------------------------------- op-signing key persistence

func TestLoadOrCreateSigningKey_CreatesThenReloadsIdentical(t *testing.T) {
	dir := t.TempDir()

	priv1, loaded, err := loadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded {
		t.Fatal("first call should have created, not loaded")
	}

	priv2, loaded, err := loadOrCreateSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded {
		t.Fatal("second call should have loaded the persisted key")
	}
	if string(priv1) != string(priv2) {
		t.Fatal("reloaded signing key differs from the original")
	}
}

// ---------------------------------------------------------------- file modes

func TestPersistedFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes not meaningful on windows")
	}
	dir := t.TempDir()
	gw, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.MintJoinToken(time.Hour); err != nil {
		t.Fatal(err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("data dir mode = %o, want 0700", perm)
	}

	for _, name := range []string{"ca.crt", "ca.key", "op-signing.key", "op-signing.pub", "tokens.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, perm)
		}
	}
}

// ---------------------------------------------------------------- minted join tokens

func TestMintedTokenExpiredRefused(t *testing.T) {
	dir := t.TempDir()
	gw, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := gw.MintJoinToken(1 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // let it expire

	_, err = gw.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-a", JoinToken: tok, CsrPem: testCSR(t, "node-a"),
	})
	if err == nil {
		t.Fatal("expired token was accepted")
	}

	// Documented semantics: an expired-but-never-used token is refused and
	// left as unused/expired in tokens.json (not marked used, not deleted —
	// see tokenStore.consume's doc comment). It must stay refused on every
	// subsequent attempt, not just the first.
	_, err = gw.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-b", JoinToken: tok, CsrPem: testCSR(t, "node-b"),
	})
	if err == nil {
		t.Fatal("expired token was accepted on retry")
	}
}

func TestMintedTokenSingleUse(t *testing.T) {
	dir := t.TempDir()
	gw, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := gw.MintJoinToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-a", JoinToken: tok, CsrPem: testCSR(t, "node-a"),
	}); err != nil {
		t.Fatalf("first join with valid token failed: %v", err)
	}
	if _, err := gw.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-b", JoinToken: tok, CsrPem: testCSR(t, "node-b"),
	}); err == nil {
		t.Fatal("token was reused")
	}
}

// TestUsedTokenPersistenceSurvivesRestart mints a token, spends it, then
// rebuilds a completely fresh Gateway from the same DataDir (simulating a
// Keystone crash/restart between "used" and the next start) and confirms
// the token stays refused.
func TestUsedTokenPersistenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	gwA, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := gwA.MintJoinToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gwA.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-a", JoinToken: tok, CsrPem: testCSR(t, "node-a"),
	}); err != nil {
		t.Fatalf("join with fresh minted token failed: %v", err)
	}

	// Fresh Gateway object, same DataDir — mirrors a process restart.
	gwB, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gwB.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-b", JoinToken: tok, CsrPem: testCSR(t, "node-b"),
	}); err == nil {
		t.Fatal("used token was accepted again after gateway restart")
	}
}

// TestStaticJoinTokensNotPersisted confirms Config.JoinTokens (the dev/test
// path) never touch tokens.json, even with DataDir set.
func TestStaticJoinTokensNotPersisted(t *testing.T) {
	dir := t.TempDir()
	gw, err := New(Config{DataDir: dir, JoinTokens: []string{"static-tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-a", JoinToken: "static-tok", CsrPem: testCSR(t, "node-a"),
	}); err != nil {
		t.Fatalf("join with static token failed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "tokens.json")); err == nil {
		t.Fatalf("static token leaked into tokens.json: %s", b)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// A fresh Gateway over the same DataDir must NOT remember the static
	// token as used — it's process-lifetime only, unlike minted tokens.
	gw2, err := New(Config{DataDir: dir, JoinTokens: []string{"static-tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw2.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-b", JoinToken: "static-tok", CsrPem: testCSR(t, "node-b"),
	}); err != nil {
		t.Fatalf("static token should be fresh again after restart (process-lifetime only): %v", err)
	}
}

// ---------------------------------------------------------------- empty DataDir behavior unchanged

func TestEmptyDataDirStaysInMemory(t *testing.T) {
	gw1, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	gw2, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if string(gw1.CA().PEM()) == string(gw2.CA().PEM()) {
		t.Fatal("two in-memory gateways unexpectedly share a CA")
	}
	if string(gw1.OpSigningPublicKey()) == string(gw2.OpSigningPublicKey()) {
		t.Fatal("two in-memory gateways unexpectedly share an op-signing key")
	}
	tok, err := gw1.MintJoinToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw1.Join(context.Background(), &wsv1.JoinRequest{
		NodeId: "node-a", JoinToken: tok, CsrPem: testCSR(t, "node-a"),
	}); err != nil {
		t.Fatalf("in-memory minted token join failed: %v", err)
	}
}
