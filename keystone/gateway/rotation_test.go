package gateway

// Unit tests for ADR-W003 W4: CA.IssueFromCSRWithLifetime and the default
// ~3-month lifetime of IssueFromCSR.

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func parseCertPEM(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("not a PEM certificate: %s", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestIssueFromCSRWithLifetimeRespectsLifetime(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := testCSR(t, "node-a")

	const lifetime = 90 * time.Second
	certPEM, err := ca.IssueFromCSRWithLifetime(csrPEM, "node-a", lifetime)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCertPEM(t, certPEM)
	if cert.Subject.CommonName != "node-a" {
		t.Fatalf("CN = %q, want node-a", cert.Subject.CommonName)
	}
	// NotBefore is backdated an hour for clock skew, so lifetime is
	// measured from now, not from NotBefore.
	got := time.Until(cert.NotAfter)
	if got > lifetime+time.Minute || got < lifetime-time.Minute {
		t.Fatalf("cert expires in %v, want approximately %v", got, lifetime)
	}
}

func TestIssueFromCSRDefaultLifetimeIsApproximatelyThreeMonths(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := testCSR(t, "node-a")

	certPEM, err := ca.IssueFromCSR(csrPEM, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCertPEM(t, certPEM)
	// Measured from now (NotBefore is backdated for clock skew).
	got := time.Until(cert.NotAfter)
	want := defaultClientCertLifetime
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 24*time.Hour {
		t.Fatalf("default cert lifetime = %v, want approximately %v (~3 months)", got, want)
	}
}
