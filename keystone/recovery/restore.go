package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// RestoreSet installs the control-plane parts of a verified whole-site set
// into a fresh data directory. CA, operation-signing keys and node identities
// are intentionally absent and will be rebuilt when Keystone starts.
func RestoreSet(setDir, dataDir, privateKey string) error {
	manifest, err := VerifySet(setDir)
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(dataDir); err == nil {
		if len(entries) != 0 {
			return fmt.Errorf("recovery: data directory is not empty")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("recovery: checking data directory: %w", err)
	}
	stateDir := filepath.Join(dataDir, "statefile")
	dbPath := filepath.Join(dataDir, "keystone.db")
	householdKeyPath := filepath.Join(dataDir, "household-secrets.key")
	importsDir := filepath.Join(dataDir, "recovery-import")
	for _, path := range []string{stateDir, dbPath, householdKeyPath, importsDir} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("recovery: destination already exists: %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if _, err := statefile.RestoreBundle(filepath.Join(setDir, "statefile.bundle"), stateDir); err != nil {
		return err
	}
	restoredDB, err := runtimedb.RestoreBackup(filepath.Join(setDir, "keystone.db"), dbPath,
		"whole-site recovery", manifest.SnapshotAt)
	if err != nil {
		_ = os.RemoveAll(stateDir)
		return err
	}
	if err := restoredDB.Close(); err != nil {
		_ = os.RemoveAll(stateDir)
		cleanupRuntimeDB(dbPath)
		return err
	}
	householdBundle, err := os.ReadFile(filepath.Join(setDir, "household-secrets.sealed"))
	if err != nil {
		_ = os.RemoveAll(stateDir)
		cleanupRuntimeDB(dbPath)
		return err
	}
	if err := runtimedb.RestoreSecretVaultKey(privateKey, householdBundle, householdKeyPath); err != nil {
		_ = os.RemoveAll(stateDir)
		cleanupRuntimeDB(dbPath)
		return err
	}
	if err := copyNodeImports(setDir, importsDir, manifest); err != nil {
		_ = os.RemoveAll(stateDir)
		cleanupRuntimeDB(dbPath)
		_ = os.Remove(householdKeyPath)
		_ = os.RemoveAll(importsDir)
		return err
	}
	return nil
}

// copyNodeImports keeps the sealed per-node material after a kit-based
// restore removes its download directory. The private key is never copied
// into the Keystone data directory.
func copyNodeImports(setDir, importsDir string, manifest Manifest) error {
	if err := os.Mkdir(importsDir, 0o700); err != nil {
		return fmt.Errorf("recovery: creating node import directory: %w", err)
	}
	files := []string{"manifest.json"}
	for _, node := range manifest.Nodes {
		files = append(files, manifest.NodeBundles[node])
	}
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(setDir, name))
		if err != nil {
			return fmt.Errorf("recovery: reading node import artifact %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(importsDir, name), body, 0o600); err != nil {
			return fmt.Errorf("recovery: preserving node import artifact %s: %w", name, err)
		}
	}
	return nil
}

// VerifySet checks the manifest and every artifact before restore changes
// anything. Only simple filenames are accepted, preventing a forged manifest
// from making verification read outside the extracted recovery directory.
func VerifySet(setDir string) (Manifest, error) {
	var manifest Manifest
	body, err := os.ReadFile(filepath.Join(setDir, "manifest.json"))
	if err != nil {
		return manifest, fmt.Errorf("recovery: reading manifest: %w", err)
	}
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.Format != "wildstone-recovery-set-v1" {
		return Manifest{}, fmt.Errorf("recovery: unsupported manifest")
	}
	for name, want := range manifest.SHA256 {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return Manifest{}, fmt.Errorf("recovery: unsafe artifact name in manifest")
		}
		body, err := os.ReadFile(filepath.Join(setDir, name))
		if err != nil {
			return Manifest{}, fmt.Errorf("recovery: reading artifact %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			return Manifest{}, fmt.Errorf("recovery: artifact %s failed integrity check", name)
		}
	}
	for _, required := range []string{"statefile.bundle", "keystone.db", "household-secrets.sealed"} {
		if _, ok := manifest.SHA256[required]; !ok {
			return Manifest{}, fmt.Errorf("recovery: manifest is missing %s", required)
		}
	}
	seenNodes := make(map[string]bool, len(manifest.Nodes))
	for _, node := range manifest.Nodes {
		if node == "" || seenNodes[node] {
			return Manifest{}, fmt.Errorf("recovery: manifest contains an invalid node list")
		}
		seenNodes[node] = true
		name, ok := manifest.NodeBundles[node]
		if !ok || name != nodeBundleName(node) {
			return Manifest{}, fmt.Errorf("recovery: manifest has no valid bundle for node %s", node)
		}
		if _, ok := manifest.SHA256[name]; !ok {
			return Manifest{}, fmt.Errorf("recovery: manifest has no integrity record for node %s", node)
		}
	}
	if len(manifest.NodeBundles) != len(manifest.Nodes) {
		return Manifest{}, fmt.Errorf("recovery: manifest contains an unexpected node bundle")
	}
	return manifest, nil
}

func cleanupRuntimeDB(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
