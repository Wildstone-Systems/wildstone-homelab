package recovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/proto/recoveryseal"
)

func TestLoadKitAndRepository(t *testing.T) {
	_, privateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	want := Kit{
		Format: "wildstone-recovery-kit-v1", Endpoint: "https://s3.example.test/", Bucket: "backups", Prefix: "home",
		ReadOnlyAccessKeyID: "read-id", ReadOnlySecretAccessKey: "read-secret",
		RepositoryPasswords: map[string]string{PlanName: "repo-password"}, RecoveryPrivateKey: privateKey,
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kit.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKit(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository(PlanName) != "s3:https://s3.example.test/backups/home/wildstone" {
		t.Fatalf("repository = %q", got.Repository(PlanName))
	}
}
