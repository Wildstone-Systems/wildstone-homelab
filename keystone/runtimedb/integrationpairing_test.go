package runtimedb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pairingVault(t *testing.T) (*DB, *SecretVault, string) {
	t.Helper()
	db, err := Open(migrated.Path(t)) // a copy of the template, not a fresh replay (#231)
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

// Unpairing forgets the pairing and revokes its publisher's tokens
// together: if the revoke fails, the pairing is still there (so it still
// shows, and can be unpaired again) — never gone with its token alive.
func TestUnpairIsAllOrNothing(t *testing.T) {
	db, v, _ := pairingVault(t)
	if err := db.CreatePrincipal("publisher-pantry", RolePublisher, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertToken("h1", "publisher-pantry", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := v.SavePairing("pantry", "https://pantry.example.internal", "pk", "sam"); err != nil {
		t.Fatal(err)
	}

	if _, err := db.sql.Exec(`CREATE TRIGGER no_revoke BEFORE UPDATE ON tokens BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Unpair("pantry", "publisher-pantry"); err == nil {
		t.Fatal("unpair succeeded though the revoke failed")
	}
	if _, err := db.Pairing("pantry"); err != nil {
		t.Fatalf("the pairing was forgotten though its token still works: %v", err)
	}

	if _, err := db.sql.Exec(`DROP TRIGGER no_revoke`); err != nil {
		t.Fatal(err)
	}
	n, err := db.Unpair("pantry", "publisher-pantry")
	if err != nil || n != 1 {
		t.Fatalf("unpair = %d %v", n, err)
	}
	if _, err := db.Pairing("pantry"); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("after unpair = %v", err)
	}
	if tok, _ := db.GetToken("h1"); tok.RevokedAt.IsZero() {
		t.Fatal("the publisher token still works after unpairing")
	}
}
