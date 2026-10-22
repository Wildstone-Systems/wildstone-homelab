package recovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/Wildstone-Systems/wildstone-homelab/proto/recoveryseal"
)

type recoveryAgent struct{ failExports bool }

func (a recoveryAgent) Apply(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if op.GetWriteManagedFile() != nil {
		return &wsv1.OpResult{Result: &wsv1.OpResult_WriteManagedFile{WriteManagedFile: &wsv1.WriteManagedFileResult{}}}, nil
	}
	if a.failExports {
		return nil, context.DeadlineExceeded
	}
	sealed, err := recoveryseal.Seal(op.GetExportSecretBundle().GetRecoveryPublicKey(), []byte("sealed-node-store:"+node))
	if err != nil {
		return nil, err
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_ExportSecretBundle{
		ExportSecretBundle: &wsv1.ExportSecretBundleResult{SealedBundle: sealed, SecretCount: 1},
	}}, nil
}

func recoveryFixture(t *testing.T, protocol uint32) (*Builder, resource.Resource, string) {
	t.Helper()
	base := t.TempDir()
	state, err := statefile.New(filepath.Join(base, "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Put(resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
		Metadata: resource.Metadata{Name: "photos", Owner: "system", Node: "node-a"},
		Volume:   &resource.VolumeSpec{Name: "photos"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("owner", "recovery-test", "seed state"); err != nil {
		t.Fatal(err)
	}
	db, err := runtimedb.Open(filepath.Join(base, "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.RecordNodeSeen("node-a", "test", protocol); err != nil {
		t.Fatal(err)
	}
	vault, err := db.OpenSecretVault(filepath.Join(base, "household-secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	publicKey, _, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "recovery")
	plan := resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindBackupPlan,
		Metadata: resource.Metadata{Name: PlanName, Owner: "system", Node: "node-a"},
		BackupPlan: &resource.BackupPlanSpec{
			Paths: []string{resource.WholeSiteRecoveryPath}, RecoveryPublicKey: publicKey,
			Offsite: &resource.BackupOffsite{Route: resource.BackupRouteOffsiteOnly},
		},
	}
	return &Builder{State: state, DB: db, Vault: vault, Agents: recoveryAgent{}, Dir: dir,
		Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }}, plan, dir
}

func TestPrepareUsesDesiredNodesAndRetainsBundleWhileActiveNodeIsOffline(t *testing.T) {
	builder, plan, dir := recoveryFixture(t, RequiredProtocolMin)
	if err := builder.DB.RecordNodeSeen("retired-node", "test", RequiredProtocolMin); err != nil {
		t.Fatal(err)
	}
	if err := builder.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	builder.Agents = recoveryAgent{failExports: true}
	if err := builder.Prepare(context.Background(), plan); err != nil {
		t.Fatalf("offline active node did not reuse its prior sealed bundle: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Nodes) != 1 || manifest.Nodes[0] != "node-a" {
		t.Fatalf("historical nodes leaked into recovery set: %+v", manifest.Nodes)
	}
}

func TestPrepareBuildsCompleteAtomicRecoverySet(t *testing.T) {
	builder, plan, dir := recoveryFixture(t, RequiredProtocolMin)
	if err := builder.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"statefile.bundle", "keystone.db", "household-secrets.sealed", nodeBundleName("node-a"), "manifest.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() == 0 {
			t.Fatalf("artifact %s: %v, size %d", name, err, func() int64 {
				if info == nil {
					return 0
				}
				return info.Size()
			}())
		}
	}
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "wildstone-recovery-set-v1" || len(manifest.Nodes) != 1 || len(manifest.SHA256) != 4 {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestPrepareKeepsPreviousSetWhenNodeIsTooOld(t *testing.T) {
	builder, plan, dir := recoveryFixture(t, RequiredProtocolMin-1)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "previous-complete-set")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := builder.Prepare(context.Background(), plan); err == nil {
		t.Fatal("old Wardstone protocol was accepted")
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "keep" {
		t.Fatalf("previous set was changed: %q, %v", body, err)
	}
}

func TestRestoreSetInstallsQuarantinedControlPlane(t *testing.T) {
	builder, plan, setDir := recoveryFixture(t, RequiredProtocolMin)
	// The plan must use the matching public half for this private key.
	publicKey, matchingPrivateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	plan.BackupPlan.RecoveryPublicKey = publicKey
	if err := builder.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "keystone-data")
	if err := RestoreSet(setDir, destination, matchingPrivateKey); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySet(setDir); err != nil {
		t.Fatalf("restore modified its source recovery set: %v", err)
	}
	restoredDB, err := runtimedb.Open(filepath.Join(destination, "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	state, err := restoredDB.GetRestoreState()
	if err != nil || !state.Quarantined {
		t.Fatalf("restore state = %+v, %v; want quarantine", state, err)
	}
	if _, err := restoredDB.OpenSecretVault(filepath.Join(destination, "household-secrets.key")); err != nil {
		t.Fatalf("restored household vault: %v", err)
	}
	importsDir := filepath.Join(destination, "recovery-import")
	for _, name := range []string{"manifest.json", nodeBundleName("node-a")} {
		if _, err := os.Stat(filepath.Join(importsDir, name)); err != nil {
			t.Fatalf("preserved node import artifact %s: %v", name, err)
		}
	}
	restoredState, err := statefile.New(filepath.Join(destination, "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := restoredState.List()
	if err != nil || len(refs) == 0 {
		t.Fatalf("restored Statefile = %+v, %v", refs, err)
	}
}

func TestRestoreSetRefusesNonFreshDataDirectory(t *testing.T) {
	builder, plan, setDir := recoveryFixture(t, RequiredProtocolMin)
	publicKey, privateKey, err := recoveryseal.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	plan.BackupPlan.RecoveryPublicKey = publicKey
	if err := builder.Prepare(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	marker := filepath.Join(destination, "ca.key")
	if err := os.WriteFile(marker, []byte("existing identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSet(setDir, destination, privateKey); err == nil {
		t.Fatal("restore into non-fresh data directory succeeded")
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "existing identity" {
		t.Fatalf("existing data was changed: %q, %v", body, err)
	}
}
