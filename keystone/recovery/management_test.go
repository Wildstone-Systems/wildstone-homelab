package recovery

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

func managementFixture(t *testing.T) (string, string, ManagementManifest) {
	t.Helper()
	root := t.TempDir()
	setDir, keyDir := filepath.Join(root, "set"), filepath.Join(root, "keys")
	if err := os.MkdirAll(setDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		t.Fatal(err)
	}

	state, err := statefile.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Put(resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindVolume,
		Metadata: resource.Metadata{Name: "synthetic", Owner: "system", Node: "node-a"},
		Volume:   &resource.VolumeSpec{Name: "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("owner", "management-recovery", "synthetic accepted state"); err != nil {
		t.Fatal(err)
	}
	head, err := state.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Bundle(filepath.Join(setDir, "statefile.bundle")); err != nil {
		t.Fatal(err)
	}

	db, err := runtimedb.Open(filepath.Join(root, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatefileHead(head); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireCoordinator("synthetic-original"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePrincipal("revoked-synthetic", runtimedb.RoleOperator, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertToken(strings.Repeat("a", 64), "revoked-synthetic", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RevokeTokensFor("revoked-synthetic"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordButlerMember("synthetic-owner", runtimedb.RoleOwner, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.AdmitButlerMember("synthetic-owner", "fixture"); err != nil {
		t.Fatal(err)
	}
	vault, err := db.OpenSecretVault(filepath.Join(setDir, "household-secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Create("synthetic-owner", runtimedb.SecretInput{Scope: "household", Label: "Synthetic recovery credential", Value: "synthetic-secret-value"}); err != nil {
		t.Fatal(err)
	}
	if err := db.BackupTo(filepath.Join(setDir, "keystone.db")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	certPEM, keyPEM := syntheticCA(t)
	writeFixture(t, setDir, "ca.crt", certPEM)
	writeFixture(t, setDir, "ca.key", keyPEM)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, setDir, "op-signing.key", priv)
	writeFixture(t, setDir, "op-signing.pub", pub)
	for name, body := range map[string]string{
		"tokens.json":            "{}\n",
		"management-values.yaml": "image:\n  digest: sha256:" + strings.Repeat("1", 64) + "\n",
		"deployment-config.json": "{\"instance\":\"synthetic\"}\n",
		"local-git-source.json":  "{\"sourceName\":\"synthetic\"}\n",
		"deployment.sops.yaml":   "synthetic: ENC[AES256_GCM,data:fixture,type:str]\n",
	} {
		writeFixture(t, setDir, name, []byte(body))
	}
	for name := range map[string]bool{"sops-age.key": true, "git-host.key": true, "git-source-client.key": true} {
		writeFixture(t, keyDir, name, []byte("synthetic-private-material-for-"+name))
	}

	promotion := filepath.Join(root, "promotion.git")
	deployment, err := statefile.OpenLocalDeployment(promotion)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"source.yaml": []byte("source\n"), "release.yaml": []byte("release\n"), "kustomization.yaml": []byte("kustomization\n")}
	revision, err := deployment.Build("", "synthetic-operation", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := deployment.Retain("synthetic-operation", revision); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Promote("", revision); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"promotion.bundle", "delivery.bundle"} {
		cmd := exec.Command("git", "--git-dir="+promotion, "bundle", "create", filepath.Join(setDir, name), "refs/heads/deploy")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git bundle: %v: %s", err, out)
		}
	}

	m := ManagementManifest{Format: ManagementRecoveryFormat, SnapshotAt: time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC),
		SourceChartVersion: "0.2.0", TargetChartVersion: "0.2.0", RuntimeLayout: "private-keystone-child",
		ImageDigest: "sha256:" + strings.Repeat("1", 64), ApprovedStatefileRevision: head,
		PromotedDeploymentRevision: revision, Quiesced: true, OriginalWriterFenced: true, OwnerApproved: true,
		Artifacts: map[string]string{}, Keys: map[string]string{}, SHA256: map[string]string{}}
	for _, name := range requiredManagementArtifacts {
		m.Artifacts[name] = name
	}
	for _, name := range requiredManagementKeys {
		m.Keys[name] = name
	}
	for logical, rel := range m.Artifacts {
		body, _ := os.ReadFile(filepath.Join(setDir, rel))
		m.SHA256[logical] = sha256Hex(body)
	}
	for logical, rel := range m.Keys {
		body, _ := os.ReadFile(filepath.Join(keyDir, rel))
		m.SHA256[logical] = sha256Hex(body)
	}
	writeManifest(t, setDir, m)
	return setDir, keyDir, m
}

func TestManagementRecoveryPreflightAcceptsCompleteIndependentSet(t *testing.T) {
	setDir, keyDir, manifest := managementFixture(t)
	report, err := VerifyManagementSet(setDir, keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.ApprovedStatefileRevision != manifest.ApprovedStatefileRevision ||
		report.PromotedDeploymentRevision != manifest.PromotedDeploymentRevision ||
		report.CoordinatorHolder != "synthetic-original" || report.CoordinatorEpoch < 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestManagementRecoveryPreflightAcceptsDeclaredChart01SourceLayout(t *testing.T) {
	setDir, keyDir, manifest := managementFixture(t)
	manifest.SourceChartVersion = "0.1.0"
	manifest.RuntimeLayout = "claim-root"
	writeManifest(t, setDir, manifest)
	if _, err := VerifyManagementSet(setDir, keyDir); err != nil {
		t.Fatal(err)
	}
}

func TestManagementRecoveryPreflightRefusesMissingUnsafeOrUnauthorizedSets(t *testing.T) {
	tests := []struct {
		name         string
		breakFixture func(*testing.T, string, string, *ManagementManifest)
		want         string
	}{
		{"missing identity", func(t *testing.T, set, _ string, _ *ManagementManifest) {
			if err := os.Remove(filepath.Join(set, "ca.key")); err != nil {
				t.Fatal(err)
			}
		}, "ca.key is missing"},
		{"missing independent key", func(t *testing.T, _, keys string, _ *ManagementManifest) {
			if err := os.Remove(filepath.Join(keys, "sops-age.key")); err != nil {
				t.Fatal(err)
			}
		}, "sops-age.key is missing"},
		{"not quiesced", func(_ *testing.T, _ string, _ string, m *ManagementManifest) { m.Quiesced = false }, "quiesce"},
		{"not Owner approved", func(_ *testing.T, _ string, _ string, m *ManagementManifest) { m.OwnerApproved = false }, "Owner approval"},
		{"chart 0.1 mislabeled", func(_ *testing.T, _ string, _ string, m *ManagementManifest) { m.SourceChartVersion = "0.1.0" }, "chart/layout"},
		{"tampered signing identity", func(t *testing.T, set, _ string, m *ManagementManifest) {
			writeFixture(t, set, "op-signing.pub", make([]byte, ed25519.PublicKeySize))
			body, _ := os.ReadFile(filepath.Join(set, "op-signing.pub"))
			m.SHA256["op-signing.pub"] = sha256Hex(body)
		}, "does not match"},
		{"mismatched household key", func(t *testing.T, set, _ string, m *ManagementManifest) {
			writeFixture(t, set, "household-secrets.key", make([]byte, 32))
			body, _ := os.ReadFile(filepath.Join(set, "household-secrets.key"))
			m.SHA256["household-secrets.key"] = sha256Hex(body)
		}, "household credential key"},
		{"invalid join-token state", func(t *testing.T, set, _ string, m *ManagementManifest) {
			writeFixture(t, set, "tokens.json", []byte("not-json"))
			body, _ := os.ReadFile(filepath.Join(set, "tokens.json"))
			m.SHA256["tokens.json"] = sha256Hex(body)
		}, "join-token state is invalid"},
		{"invalid join-token record", func(t *testing.T, set, _ string, m *ManagementManifest) {
			writeFixture(t, set, "tokens.json", []byte(`{"`+strings.Repeat("a", 64)+`":null}`))
			body, _ := os.ReadFile(filepath.Join(set, "tokens.json"))
			m.SHA256["tokens.json"] = sha256Hex(body)
		}, "join-token state is invalid"},
		{"missing configured provider keys", func(_ *testing.T, _ string, _ string, m *ManagementManifest) { m.ProviderKeysRequired = true }, "provider-keys.json is missing"},
		{"missing enabled binding", func(_ *testing.T, _ string, _ string, m *ManagementManifest) { m.SharedUploadsRequired = true }, "shared-uploads is missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setDir, keyDir, manifest := managementFixture(t)
			tt.breakFixture(t, setDir, keyDir, &manifest)
			writeManifest(t, setDir, manifest)
			if _, err := VerifyManagementSet(setDir, keyDir); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRestoreManagementSetPublishesQuarantinedPreservedRuntime(t *testing.T) {
	setDir, keyDir, manifest := managementFixture(t)
	target := filepath.Join(t.TempDir(), "keystone")
	report, err := RestoreManagementSet(setDir, keyDir, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.ApprovedStatefileRevision != manifest.ApprovedStatefileRevision {
		t.Fatalf("restored report = %+v", report)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("target mode = %o, want 0700", info.Mode().Perm())
	}

	restoredDB, err := runtimedb.Open(filepath.Join(target, "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	state, err := restoredDB.GetRestoreState()
	if err != nil || !state.Quarantined {
		t.Fatalf("target quarantine = %+v, err = %v", state, err)
	}
	head, err := restoredDB.StatefileHead()
	if err != nil || head != manifest.ApprovedStatefileRevision {
		t.Fatalf("target Statefile head = %q, err = %v", head, err)
	}
	lease, err := restoredDB.GetCoordinator()
	if err != nil || lease.Holder != "synthetic-original" || lease.Epoch < 1 {
		t.Fatalf("target coordinator = %+v, err = %v", lease, err)
	}
	if _, err := restoredDB.OpenSecretVault(filepath.Join(target, "household-secrets.key")); err != nil {
		t.Fatalf("restored household key does not open preserved credentials: %v", err)
	}

	for _, name := range []string{"ca.crt", "ca.key", "op-signing.key", "op-signing.pub", "tokens.json", "household-secrets.key"} {
		want, err := os.ReadFile(filepath.Join(setDir, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s was not preserved byte-for-byte", name)
		}
	}
	restoredState, err := statefile.New(filepath.Join(target, "statefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !restoredState.HasCommit(manifest.ApprovedStatefileRevision) {
		t.Fatal("restored Statefile lacks accepted revision")
	}

	sourceDB, err := runtimedb.Open(filepath.Join(setDir, "keystone.db"))
	if err != nil {
		t.Fatal(err)
	}
	sourceState, stateErr := sourceDB.GetRestoreState()
	closeErr := sourceDB.Close()
	if stateErr != nil || sourceState.Quarantined || closeErr != nil {
		t.Fatalf("source recovery database was modified: state=%+v stateErr=%v closeErr=%v", sourceState, stateErr, closeErr)
	}
}

func TestRestoreManagementSetRefusesExistingTarget(t *testing.T) {
	setDir, keyDir, _ := managementFixture(t)
	target := filepath.Join(t.TempDir(), "keystone")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "owner-data")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreManagementSet(setDir, keyDir, target); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing target was not refused: %v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "preserve" {
		t.Fatalf("existing target was changed: body=%q err=%v", body, err)
	}
}

func TestLegacyWholeSiteSetIsNotAcceptedAsManagementRecovery(t *testing.T) {
	setDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(setDir, "manifest.json"), []byte(`{"format":"wildstone-recovery-set-v1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyManagementSet(setDir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "management-manifest.json") {
		t.Fatalf("legacy set was accepted: %v", err)
	}
}

func syntheticCA(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-management-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFixture(t *testing.T, root, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, setDir string, manifest ManagementManifest) {
	t.Helper()
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(setDir, "management-manifest.json"), append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
