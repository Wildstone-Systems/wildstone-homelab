package gateway

// Unit test for ADR-W003 W3: CA.Fingerprint().

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"regexp"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestCAFingerprintFormat(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := ca.Fingerprint()
	if !hex64.MatchString(fp) {
		t.Fatalf("fingerprint %q is not 64 lowercase hex chars", fp)
	}

	block, _ := pem.Decode(ca.PEM())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("ca.PEM() did not decode to a CERTIFICATE block")
	}
	want := sha256.Sum256(block.Bytes)
	if fp != hex.EncodeToString(want[:]) {
		t.Fatalf("fingerprint %q does not match sha256(cert DER) %x", fp, want)
	}

	// Stable across calls.
	if fp2 := ca.Fingerprint(); fp2 != fp {
		t.Fatalf("Fingerprint() not stable: %q vs %q", fp, fp2)
	}
}

func TestServerTLSCertIncludesCAInChain(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServerTLSCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) != 2 {
		t.Fatalf("expected [leaf, ca], got %d certs in chain", len(cert.Certificate))
	}
	block, _ := pem.Decode(ca.PEM())
	if string(cert.Certificate[1]) != string(block.Bytes) {
		t.Fatalf("chain[1] is not the CA certificate DER")
	}
}
