package agent

// Integration tests for ADR-W003 W4: certificate rotation. Same real-gateway
// + real-agent harness as integration_test.go.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// loadCertFile parses a PEM-encoded certificate file (node.crt layout) for
// assertions on NotAfter.
func loadCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("%s: not a PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// TestCertificateRotation exercises the full ADR-W003 W4 loop: an agent
// joins normally, is handed a short-lived certificate (via the CA's
// IssueFromCSRWithLifetime test hook, reusing the agent's already-persisted
// key), detects on its next rotation check that under 1/3 of the cert's
// lifetime remains, renews over its live authenticated connection, persists
// the new key+cert, and forces a reconnect. Asserts the gateway sees the
// reconnected node, the on-disk cert's NotAfter moved later, and a Health op
// still works afterwards.
func TestCertificateRotation(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	dataDir := filepath.Join(t.TempDir(), "data")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.DataDir = dataDir
		// Speed up the periodic rotation check so the test doesn't wait 24h
		// for the tick that notices the planted short-lived cert.
		c.RenewCheckInterval = 200 * time.Millisecond
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	certPath := filepath.Join(dataDir, "node.crt")
	keyPath := filepath.Join(dataDir, "node.key")

	_ = loadCertFile(t, certPath) // sanity: original cert parses before we plant the short one

	// Reuse the agent's already-persisted private key to build a CSR, then
	// have the gateway's CA (test hook) issue a ~90-second certificate for
	// it, and overwrite node.crt on disk. The agent's rotation checker reads
	// this file, not the in-memory TLS state of the live connection, so this
	// takes effect on the next check without disturbing the live session.
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("node.key: not PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-a"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	shortCertPEM, err := h.gw.CA().IssueFromCSRWithLifetime(csrPEM, "node-a", 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, shortCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// Wait for the agent to notice, renew, and reconnect: node.crt's
	// NotAfter should move from the planted ~90-second horizon back out to
	// the standard ~3-month lifetime the real Renew RPC issues. (Comparing
	// against the ORIGINAL cert's NotAfter would be wrong — a renewed
	// 3-month cert expires within seconds of the original 3-month cert.)
	planted := loadCertFile(t, certPath).NotAfter
	deadline := time.Now().Add(20 * time.Second)
	var renewedCert *x509.Certificate
	for time.Now().Before(deadline) {
		c := loadCertFile(t, certPath)
		if c.NotAfter.After(planted.Add(24 * time.Hour)) {
			renewedCert = c
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if renewedCert == nil {
		t.Fatalf("cert was not renewed within deadline (still: NotAfter=%v)", loadCertFile(t, certPath).NotAfter)
	}
	if renewedCert.Subject.CommonName != "node-a" {
		t.Fatalf("renewed cert CN = %q, want node-a", renewedCert.Subject.CommonName)
	}

	// (a) gateway sees it connected (post-renewal reconnect completed).
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent did not reconnect with the renewed certificate")
	}

	// (c) a Health op works post-renewal.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_Health{Health: &wsv1.Health{}}})
	if hres := res.GetHealth(); hres == nil || !hres.GetOk() {
		t.Fatalf("post-renewal health: %+v", res)
	}
}

// TestRenewWithoutClientCertRefused: calling Renew on a connection dialed
// with only RootCAs (no client certificate) must be refused as
// Unauthenticated — the same per-RPC verified-client-cert requirement as
// Session (ADR-W003 A5).
func TestRenewWithoutClientCertRefused(t *testing.T) {
	h := startGateway(t, nil, "")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(h.gw.CA().PEM()) {
		t.Fatal("failed to load CA pool")
	}
	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-a"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = wsv1.NewKeystoneGatewayClient(conn).Renew(ctx, &wsv1.RenewRequest{CsrPem: csrPEM})
	if err == nil {
		t.Fatal("Renew without a client certificate succeeded, want Unauthenticated")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Renew without client cert: got code %v, want Unauthenticated (%v)", status.Code(err), err)
	}
}

// TestRenewCannotChangeCN: a CSR with a different Subject.CommonName renewed
// over an authenticated node's connection must still be issued with that
// node's original CN — identity comes from the mTLS peer cert, never the
// CSR (ADR-W003 A5, W4).
func TestRenewCannotChangeCN(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	dataDir := filepath.Join(t.TempDir(), "data")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DataDir = dataDir })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// A fresh connection authenticated as node-a (separate from the agent's
	// own live session — Renew is a per-connection unary RPC, not tied to
	// the streaming Session).
	cert, err := tls.LoadX509KeyPair(filepath.Join(dataDir, "node.crt"), filepath.Join(dataDir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(h.gw.CA().PEM()) {
		t.Fatal("failed to load CA pool")
	}
	conn, err := grpc.NewClient(h.addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	evilKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "evil-node"},
	}, evilKey)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := wsv1.NewKeystoneGatewayClient(conn).Renew(ctx, &wsv1.RenewRequest{CsrPem: csrPEM})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	issued := loadCertFile(t, certFromPEMToTempFile(t, resp.GetCertPem()))
	if issued.Subject.CommonName != "node-a" {
		t.Fatalf("issued cert CN = %q, want node-a (CSR CN must be ignored)", issued.Subject.CommonName)
	}
}

// certFromPEMToTempFile writes certPEM to a temp file and returns its path,
// so loadCertFile (which reads from disk, matching how the agent persists
// certs) can be reused for a directly-parsed response too.
func certFromPEMToTempFile(t *testing.T, certPEM []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "issued.crt")
	if err := os.WriteFile(p, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
