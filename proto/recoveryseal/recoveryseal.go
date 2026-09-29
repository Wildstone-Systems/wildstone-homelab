// Package recoveryseal implements the portable sealed envelope used by the
// whole-site recovery kit (ADR-W045 A5). It intentionally depends only on the
// Go standard library so both Keystone and Wardstone can use the exact same
// format without creating a module dependency in either direction.
package recoveryseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	publicPrefix  = "WSR-PUBLIC-1-"
	privatePrefix = "WSR-PRIVATE-1-"
	magic         = "WSR1"
)

var ErrOpen = errors.New("recoveryseal: bundle could not be opened")

// GenerateKeyPair returns printable X25519 keys. The private key belongs only
// in the recovery kit; the public key is safe to commit to the Statefile.
func GenerateKeyPair() (publicKey, privateKey string, err error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return encodeKey(publicPrefix, priv.PublicKey().Bytes()), encodeKey(privatePrefix, priv.Bytes()), nil
}

func encodeKey(prefix string, raw []byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeKey(value, prefix string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(value), prefix)
	if !ok {
		return nil, fmt.Errorf("recoveryseal: key must begin with %s", prefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("recoveryseal: invalid key")
	}
	return raw, nil
}

// ValidatePublicKey checks the Statefile-safe half without sealing data.
func ValidatePublicKey(value string) error {
	raw, err := decodeKey(value, publicPrefix)
	if err != nil {
		return err
	}
	curve := ecdh.X25519()
	publicKey, err := curve.NewPublicKey(raw)
	if err != nil {
		return err
	}
	probe, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	shared, err := probe.ECDH(publicKey)
	clear(shared)
	return err
}

// ValidatePrivateKey checks the offline half without opening data.
func ValidatePrivateKey(value string) error {
	raw, err := decodeKey(value, privatePrefix)
	if err != nil {
		return err
	}
	defer clear(raw)
	_, err = ecdh.X25519().NewPrivateKey(raw)
	return err
}

// Seal encrypts plaintext to the public recovery key using ephemeral X25519
// and AES-256-GCM. The recipient key and envelope header are authenticated.
func Seal(publicKey string, plaintext []byte) ([]byte, error) {
	recipientRaw, err := decodeKey(publicKey, publicPrefix)
	if err != nil {
		return nil, err
	}
	curve := ecdh.X25519()
	recipient, err := curve.NewPublicKey(recipientRaw)
	if err != nil {
		return nil, err
	}
	ephemeral, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, err
	}
	defer clear(shared)
	ephemeralRaw := ephemeral.PublicKey().Bytes()
	key := derive(shared, ephemeralRaw, recipientRaw)
	defer clear(key[:])
	aead, err := newAEAD(key[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := append(append([]byte(magic), ephemeralRaw...), nonce...)
	aad := append(append([]byte{}, header...), recipientRaw...)
	return append(header, aead.Seal(nil, nonce, plaintext, aad)...), nil
}

// Open decrypts a bundle with the recovery private key. All parse, wrong-key
// and authentication failures collapse to ErrOpen so callers never turn a
// recovery oracle into detailed attacker feedback.
func Open(privateKey string, bundle []byte) ([]byte, error) {
	privateRaw, err := decodeKey(privateKey, privatePrefix)
	if err != nil {
		return nil, ErrOpen
	}
	defer clear(privateRaw)
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(privateRaw)
	if err != nil {
		return nil, ErrOpen
	}
	const ephemeralSize = 32
	nonceSize := 12
	if len(bundle) < len(magic)+ephemeralSize+nonceSize || string(bundle[:len(magic)]) != magic {
		return nil, ErrOpen
	}
	ephemeralRaw := bundle[len(magic) : len(magic)+ephemeralSize]
	ephemeral, err := curve.NewPublicKey(ephemeralRaw)
	if err != nil {
		return nil, ErrOpen
	}
	shared, err := priv.ECDH(ephemeral)
	if err != nil {
		return nil, ErrOpen
	}
	defer clear(shared)
	recipientRaw := priv.PublicKey().Bytes()
	key := derive(shared, ephemeralRaw, recipientRaw)
	defer clear(key[:])
	aead, err := newAEAD(key[:])
	if err != nil {
		return nil, ErrOpen
	}
	headerLen := len(magic) + ephemeralSize + aead.NonceSize()
	header := bundle[:headerLen]
	nonce := header[len(magic)+ephemeralSize:]
	aad := append(append([]byte{}, header...), recipientRaw...)
	plaintext, err := aead.Open(nil, nonce, bundle[headerLen:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

func derive(shared, ephemeral, recipient []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("wildstone-recovery-seal-v1\x00"))
	h.Write(shared)
	h.Write(ephemeral)
	h.Write(recipient)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
