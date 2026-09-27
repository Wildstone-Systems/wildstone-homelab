package runtimedb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func pairingVault(t *testing.T) (*DB, *SecretVault, string) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keyPath := filepath.Join(t.TempDir(), "household.key")
	v, err := db.OpenSecretVault(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return db, v, keyPath
}

func TestAPairedCredentialIsSealedAndRotatedByPairingAgain(t *testing.T) {
	db, v, _ := pairingVault(t)
	if _, _, err := v.PairingCredential("pantry"); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("unpaired = %v", err)
	}
	if err := v.SavePairing("pantry", "https://pantry.example.internal", "pk_first", "sam"); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := db.sql.QueryRow(`SELECT key_ciphertext FROM integration_pairings WHERE capability='pantry'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("pk_first")) {
		t.Fatal("the credential is stored in plaintext")
	}
	endpoint, key, err := v.PairingCredential("pantry")
	if err != nil || endpoint != "https://pantry.example.internal" || key != "pk_first" {
		t.Fatalf("credential = %q %q %v", endpoint, key, err)
	}
	info, err := db.Pairing("pantry")
	if err != nil || info.PairedBy != "sam" || info.Endpoint != endpoint {
		t.Fatalf("info = %+v %v", info, err)
	}

	if err := v.SavePairing("pantry", "https://pantry2.example.internal", "pk_second", "sam"); err != nil {
		t.Fatal(err)
	}
	if _, key, _ := v.PairingCredential("pantry"); key != "pk_second" {
		t.Fatalf("after re-pairing key = %q", key)
	}
	if err := db.DeletePairing("pantry"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.PairingCredential("pantry"); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("after delete = %v", err)
	}
}

// A sealed credential moved to another endpoint does not open: the
// endpoint is bound into the ciphertext, so a row edited to point the key
// somewhere else is refused rather than sent there.
func TestAPairedCredentialIsBoundToItsEndpoint(t *testing.T) {
	db, v, _ := pairingVault(t)
	if err := v.SavePairing("pantry", "https://pantry.example.internal", "pk_first", "sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE integration_pairings SET endpoint='https://elsewhere.example.internal'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.PairingCredential("pantry"); err == nil {
		t.Fatal("a credential opened after its endpoint was changed")
	}
}

func TestTheVaultRefusesAMissingOrWrongKeyWhenOnlyAPairingExists(t *testing.T) {
	db, v, keyPath := pairingVault(t)
	if err := v.SavePairing("pantry", "https://pantry.example.internal", "pk_first", "sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenSecretVault(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Fatal("a missing key was replaced while a pairing is stored")
	}
	wrong := make([]byte, 32)
	if err := os.WriteFile(keyPath, wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OpenSecretVault(keyPath); err == nil {
		t.Fatal("a wrong key was accepted with a pairing stored")
	}
}
