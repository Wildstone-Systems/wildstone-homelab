package pkgtool

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// pkgSignDomainPrefix domain-separates package signatures from every
// other ed25519 use in the system (the ADR-W003 W1 pattern).
const pkgSignDomainPrefix = "wildstone-pkg-v1"

// signPayload is the byte string actually signed: the domain prefix plus
// the archive's hex sha256, hashed once more so the signed message has a
// fixed shape.
func signPayload(archiveSha256Hex string) []byte {
	h := sha256.New()
	h.Write([]byte(pkgSignDomainPrefix))
	h.Write([]byte(archiveSha256Hex))
	return h.Sum(nil)
}

// SignArchive signs an archive digest (as returned by BuildArchive) with
// the publisher's ed25519 key.
func SignArchive(priv ed25519.PrivateKey, archiveSha256Hex string) []byte {
	return ed25519.Sign(priv, signPayload(archiveSha256Hex))
}

// VerifyArchive reports whether sig is the publisher's signature over the
// archive digest.
func VerifyArchive(pub ed25519.PublicKey, archiveSha256Hex string, sig []byte) bool {
	return ed25519.Verify(pub, signPayload(archiveSha256Hex), sig)
}

// GenerateSigningKey creates a publisher keypair and writes it as two hex
// files: <path> (private, 0600) and <path>.pub (public, 0644). MVP has
// exactly one trusted publisher — the owner (ADR-W010 A2).
func GenerateSigningKey(path string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("pkgtool: generating key: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(priv)), 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(path+".pub", []byte(hex.EncodeToString(pub)), 0o644); err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}

// LoadPrivateKey reads a hex ed25519 private key written by
// GenerateSigningKey.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	return loadHexKey(path, ed25519.PrivateKeySize)
}

// LoadPublicKey reads a hex ed25519 public key (<keyfile>.pub).
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	key, err := loadHexKey(path, ed25519.PublicKeySize)
	return ed25519.PublicKey(key), err
}

func loadHexKey(path string, wantLen int) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(string(bytes.TrimSpace(data)))
	if err != nil {
		return nil, fmt.Errorf("pkgtool: key %s is not hex: %w", path, err)
	}
	if len(key) != wantLen {
		return nil, fmt.Errorf("pkgtool: key %s has %d bytes, want %d", path, len(key), wantLen)
	}
	return key, nil
}
