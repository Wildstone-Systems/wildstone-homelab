package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

type recoveryAPIAgent struct{ op *wsv1.Operation }

func (a *recoveryAPIAgent) Apply(_ context.Context, _ string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	a.op = op
	return &wsv1.OpResult{Result: &wsv1.OpResult_ImportSecretBundle{
		ImportSecretBundle: &wsv1.ImportSecretBundleResult{SecretCount: 3},
	}}, nil
}

func TestImportNodeRecoveryPassesOnlySealedMaterialToWardstone(t *testing.T) {
	db, err := runtimedb.Open(migrated.Into(t, filepath.Join(t.TempDir(), "runtime.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	agent := &recoveryAPIAgent{}
	srv := &Server{DB: db, Agents: agent}
	recoveryKey := "WSR-" + "PRIVATE-1-test"
	body, _ := json.Marshal(map[string]any{
		"privateKey": recoveryKey, "sealedBundle": []byte("opaque-ciphertext"),
	})
	req := httptest.NewRequest("POST", "/api/v1/nodes/node-a/recovery/secrets", bytes.NewReader(body))
	req.SetPathValue("node", "node-a")
	out, status, err := srv.importNodeRecovery(req, principal.Identity{Name: "owner"})
	if err != nil || status != 200 {
		t.Fatalf("import = %+v, %d, %v", out, status, err)
	}
	op := agent.op.GetImportSecretBundle()
	if op.GetRecoveryPrivateKey() != recoveryKey || string(op.GetSealedBundle()) != "opaque-ciphertext" {
		t.Fatalf("Wardstone op = %+v", op)
	}
	if out.(map[string]any)["secretCount"] != uint32(3) {
		t.Fatalf("response = %+v", out)
	}
	if imported, err := db.RecoveryNodeSecretsImported("node-a"); err != nil || !imported {
		t.Fatalf("recovery import marker = %v, %v", imported, err)
	}
}
