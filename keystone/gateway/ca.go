package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// CA is Keystone's embedded certificate authority. The CA cert/key are
// persisted in the Keystone data dir (loadCA/KeyPEM, ADR-W003 W2) so agents
// that pinned the CA keep validating it across restarts.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		// Random serial, like the leaf certs — a fixed serial made two CA
		// generations (two data dirs) collide on serial 1, which confuses
		// some cert tooling.
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "wildstone-keystone-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

func (ca *CA) PEM() []byte { return ca.pem }

// KeyPEM PEM-encodes the CA's ECDSA private key ("EC PRIVATE KEY"), for W2
// persistence (ca.key in Keystone's data dir). Callers must write it 0600.
func (ca *CA) KeyPEM() []byte {
	der, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		panic(err) // an ecdsa.PrivateKey we generated/parsed ourselves can't fail to marshal
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// loadCA reconstructs a CA from previously-persisted PEM (ca.crt, ca.key —
// ADR-W003 W2). Used on Keystone restart so agents that pinned the old CA
// keep validating it.
func loadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("ca.crt: not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca.crt: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("ca.key: not a PEM EC private key")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca.key: %w", err)
	}
	return &CA{cert: cert, key: key, pem: certPEM}, nil
}

// Fingerprint returns the hex-encoded sha256 of the CA certificate's DER
// bytes (ADR-W003 W3). This is the value operators pin agents to via
// --ca-fingerprint at join, closing the join-time MITM window that TOFU
// leaves open. Printed at Keystone startup and surfaced for the installer
// (E10).
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// ServerTLSCert issues a serving certificate for the gateway itself. The
// returned chain includes the CA certificate after the leaf (ADR-W003 W3):
// servers are permitted to send intermediates/roots along with the leaf,
// and clients performing CA-fingerprint pinning need to see the CA in the
// handshake (crypto/tls's rawCerts / ConnectionState.PeerCertificates)
// without first having a trusted pool to verify against. This does not
// change trust for existing verifying clients — RootCAs-based verification
// tolerates the extra self-signed cert in the presented chain.
func (ca *CA) ServerTLSCert(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: "wildstone-keystone"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}, nil
}

// defaultClientCertLifetime is the standard node client-cert lifetime,
// deliberately short to force rotation work (ADR-W003 A5, W4) before it
// becomes an outage.
const defaultClientCertLifetime = 3 * 30 * 24 * time.Hour // ~3 months

// IssueFromCSR signs a node client certificate with the default ~3-month
// lifetime. The certificate CN is forced to the node ID — the CN is the
// identity the Session handler trusts, so it comes from the join exchange
// (or, for renewal, the mTLS peer cert), never from the CSR subject.
func (ca *CA) IssueFromCSR(csrPEM []byte, nodeID string) ([]byte, error) {
	return ca.IssueFromCSRWithLifetime(csrPEM, nodeID, defaultClientCertLifetime)
}

// IssueFromCSRWithLifetime is IssueFromCSR with an explicit lifetime. Test
// hook (ADR-W003 W4): integration tests use it to mint short-lived certs so
// rotation can be exercised without waiting three months. Production code
// paths always go through IssueFromCSR's default.
func (ca *CA) IssueFromCSRWithLifetime(csrPEM []byte, nodeID string, lifetime time.Duration) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("not a CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	if err := validateCSRPublicKey(csr.PublicKey); err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// validateCSRPublicKey refuses a node key too weak (or of an unexpected
// type) to sign a client certificate for. Without this the CA would happily
// issue a cert for, e.g., a 512-bit RSA key. The node agent uses ECDSA
// P-256 (see wardstone signing/join); RSA is accepted at >= 2048 bits for
// interop, everything else is rejected.
func validateCSRPublicKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256(), elliptic.P384(), elliptic.P521():
			return nil
		default:
			return fmt.Errorf("CSR public key: unsupported elliptic curve")
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return fmt.Errorf("CSR public key: RSA key too small (%d bits, need >= 2048)", k.N.BitLen())
		}
		return nil
	default:
		return fmt.Errorf("CSR public key: unsupported type %T (want ECDSA or RSA)", pub)
	}
}

func newSerial() *big.Int {
	s, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err) // rand.Reader failure is unrecoverable
	}
	return s
}
