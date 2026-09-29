package runtimedb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

const householdRecoveryKeyMagic = "wildstone-household-key-v1\x00"

// ExportRecoveryBundle seals the household vault key to the offline recovery
// public key. The runtime database snapshot already contains every encrypted
// value, grant and audit row, so preserving this key restores the domain
// exactly without creating a second plaintext-value export path.
func (v *SecretVault) ExportRecoveryBundle(publicKey string) ([]byte, error) {
	key, err := os.ReadFile(v.keyPath)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: reading household secret key for recovery: %w", err)
	}
	defer clear(key)
	if len(key) != 32 {
		return nil, errors.New("runtimedb: invalid household secret key")
	}
	plaintext := make([]byte, len(householdRecoveryKeyMagic)+len(key))
	copy(plaintext, householdRecoveryKeyMagic)
	copy(plaintext[len(householdRecoveryKeyMagic):], key)
	defer clear(plaintext)
	sealed, err := recoveryseal.Seal(publicKey, plaintext)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: invalid recovery public key")
	}
	return sealed, nil
}

// RestoreSecretVaultKey opens a household recovery bundle directly into a
// fresh key path. It refuses overwrite, writes with owner-only permissions,
// and never exposes the key through a CLI/API return value.
func RestoreSecretVaultKey(privateKey string, bundle []byte, keyPath string) error {
	if _, err := os.Stat(keyPath); err == nil {
		return errors.New("runtimedb: household secret key already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("runtimedb: checking household secret key path: %w", err)
	}
	plaintext, err := recoveryseal.Open(privateKey, bundle)
	if err != nil {
		return errors.New("runtimedb: household recovery bundle could not be opened")
	}
	defer clear(plaintext)
	if len(plaintext) != len(householdRecoveryKeyMagic)+32 ||
		string(plaintext[:len(householdRecoveryKeyMagic)]) != householdRecoveryKeyMagic {
		return errors.New("runtimedb: invalid household recovery bundle")
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return fmt.Errorf("runtimedb: creating household key directory: %w", err)
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("runtimedb: creating household secret key: %w", err)
	}
	_, writeErr := f.Write(plaintext[len(householdRecoveryKeyMagic):])
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("runtimedb: writing household secret key: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("runtimedb: closing household secret key: %w", closeErr)
	}
	return nil
}
