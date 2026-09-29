package recoveryseal

import (
	"bytes"
	"errors"
	"testing"
)

func TestRoundTripAndWrongKey(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("node secrets never cross this boundary in plaintext")
	bundle, err := Seal(pub, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bundle, plaintext) {
		t.Fatal("sealed bundle contains plaintext")
	}
	opened, err := Open(priv, bundle)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("Open = %q, %v", opened, err)
	}
	_, wrong, _ := GenerateKeyPair()
	if _, err := Open(wrong, bundle); !errors.Is(err, ErrOpen) {
		t.Fatalf("wrong key error = %v", err)
	}
	bundle[len(bundle)-1] ^= 1
	if _, err := Open(priv, bundle); !errors.Is(err, ErrOpen) {
		t.Fatalf("tamper error = %v", err)
	}
}
