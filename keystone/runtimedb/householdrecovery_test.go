package runtimedb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

func TestHouseholdRecoveryBundleRestoresEncryptedDomain(t *testing.T) {
	db, vault, sourceKeyPath := pairingVault(t)
	if err := vault.SavePairing("pantry", "https://pantry.internal", "pairing-secret-value", "owner"); err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sealedKey, err := vault.ExportRecoveryBundle(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, err := os.ReadFile(sourceKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealedKey, sourceKey) {
		t.Fatal("household recovery bundle contains the plaintext vault key")
	}
	snapshot := filepath.Join(t.TempDir(), "runtime.snapshot.db")
	if err := db.BackupTo(snapshot); err != nil {
		t.Fatal(err)
	}
	restoredDB, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	restoredKeyPath := filepath.Join(t.TempDir(), "household-secrets.key")
	if err := RestoreSecretVaultKey(privateKey, sealedKey, restoredKeyPath); err != nil {
		t.Fatal(err)
	}
	restoredVault, err := restoredDB.OpenSecretVault(restoredKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, value, err := restoredVault.PairingCredential("pantry")
	if err != nil || endpoint != "https://pantry.internal" || value != "pairing-secret-value" {
		t.Fatalf("restored pairing = %q %q, %v", endpoint, value, err)
	}
	if err := RestoreSecretVaultKey(privateKey, sealedKey, restoredKeyPath); err == nil {
		t.Fatal("household key overwrite was accepted")
	}
}
