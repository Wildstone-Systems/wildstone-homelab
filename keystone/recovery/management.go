package recovery

// This file defines the fail-closed, repository-level preflight for restoring
// the same Keystone management installation. It is deliberately distinct from
// RestoreSet: the older whole-site path rebuilds the CA and operation-signing
// identity and is therefore not a same-installation recovery path.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/gateway"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

const ManagementRecoveryFormat = "wildstone-management-recovery-v1"

var managementRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// managementChartLayouts and managementChartTransitions are intentionally
// explicit. A chart release may change coordinator identity, probes or other
// packaging without changing the on-disk layout, but recovery must not infer
// compatibility from SemVer or accept a future version it has not reviewed.
var managementChartLayouts = map[string]string{
	"0.1.0": "claim-root",
	"0.2.0": "private-keystone-child",
	"0.2.1": "private-keystone-child",
}

var managementChartTransitions = map[string]map[string]bool{
	"0.1.0": {"0.2.0": true},
	"0.2.0": {"0.2.0": true, "0.2.1": true},
	"0.2.1": {"0.2.1": true},
}

// ManagementManifest binds a quiesced consistency group. Artifact keys are
// fixed logical names; their values are relative regular-file paths beneath
// either the extracted recovery set or the separately held key directory.
type ManagementManifest struct {
	Format                     string            `json:"format"`
	SnapshotAt                 time.Time         `json:"snapshotAt"`
	SourceChartVersion         string            `json:"sourceChartVersion"`
	TargetChartVersion         string            `json:"targetChartVersion"`
	RuntimeLayout              string            `json:"runtimeLayout"`
	ImageDigest                string            `json:"imageDigest"`
	ApprovedStatefileRevision  string            `json:"approvedStatefileRevision"`
	PromotedDeploymentRevision string            `json:"promotedDeploymentRevision"`
	Quiesced                   bool              `json:"quiesced"`
	OriginalWriterFenced       bool              `json:"originalWriterFenced"`
	OwnerApproved              bool              `json:"ownerApproved"`
	ProviderKeysRequired       bool              `json:"providerKeysRequired"`
	PolicyExemptionsRequired   bool              `json:"policyExemptionsRequired"`
	SharedUploadsRequired      bool              `json:"sharedUploadsRequired"`
	PrivateWorkspacesRequired  bool              `json:"privateWorkspacesRequired"`
	Artifacts                  map[string]string `json:"artifacts"`
	Keys                       map[string]string `json:"keys"`
	SHA256                     map[string]string `json:"sha256"`
}

type ManagementPreflight struct {
	SnapshotAt                 time.Time
	ApprovedStatefileRevision  string
	PromotedDeploymentRevision string
	CoordinatorHolder          string
	CoordinatorEpoch           int64
}

var requiredManagementArtifacts = []string{
	"statefile.bundle", "keystone.db", "ca.crt", "ca.key",
	"op-signing.key", "op-signing.pub", "tokens.json",
	"household-secrets.key", "management-values.yaml",
	"deployment-config.json", "local-git-source.json", "deployment.sops.yaml",
	"promotion.bundle", "delivery.bundle",
}

var requiredManagementKeys = []string{
	"sops-age.key", "git-host.key", "git-source-client.key", "recovery-journal.pub",
}

// VerifyManagementSet performs only local reads of the supplied material and
// writes temporary verification copies. It never starts Keystone, changes a
// Git ref, contacts Kubernetes, or promotes a coordinator.
func VerifyManagementSet(setDir, keyDir string) (ManagementPreflight, error) {
	var report ManagementPreflight
	setRoot, err := filepath.EvalSymlinks(setDir)
	if err != nil {
		return report, fmt.Errorf("management recovery: resolving set directory: %w", err)
	}
	keyRoot, err := filepath.EvalSymlinks(keyDir)
	if err != nil {
		return report, fmt.Errorf("management recovery: resolving key directory: %w", err)
	}
	if pathsOverlap(setRoot, keyRoot) {
		return report, fmt.Errorf("management recovery: recovery set and separately held key directories must not overlap")
	}
	setDir, keyDir = setRoot, keyRoot
	manifestPath := filepath.Join(setDir, "management-manifest.json")
	if info, err := os.Lstat(manifestPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return report, fmt.Errorf("management recovery: management-manifest.json is missing or not a regular file")
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return report, fmt.Errorf("management recovery: reading manifest: %w", err)
	}
	var m ManagementManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return report, fmt.Errorf("management recovery: decoding manifest: %w", err)
	}
	if m.Format != ManagementRecoveryFormat || m.SnapshotAt.IsZero() {
		return report, fmt.Errorf("management recovery: unsupported or incomplete manifest")
	}
	if !m.Quiesced || !m.OriginalWriterFenced || !m.OwnerApproved {
		return report, fmt.Errorf("management recovery: quiesce, original-writer fencing, and Owner approval are all required")
	}
	wantLayout := managementChartLayouts[m.SourceChartVersion]
	if wantLayout == "" || m.RuntimeLayout != wantLayout ||
		!managementChartTransitions[m.SourceChartVersion][m.TargetChartVersion] {
		return report, fmt.Errorf("management recovery: unsupported chart/layout transition")
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(m.ImageDigest) {
		return report, fmt.Errorf("management recovery: immutable management image digest is required")
	}
	if !managementRevision.MatchString(m.ApprovedStatefileRevision) || !managementRevision.MatchString(m.PromotedDeploymentRevision) {
		return report, fmt.Errorf("management recovery: approved Statefile and promoted deployment revisions are required")
	}
	for _, name := range requiredManagementArtifacts {
		if _, err := verifiedArtifact(setDir, name, m.Artifacts[name], m.SHA256); err != nil {
			return report, err
		}
	}
	for _, name := range requiredManagementKeys {
		if _, err := verifiedArtifact(keyDir, name, m.Keys[name], m.SHA256); err != nil {
			return report, err
		}
	}
	journalPub, _ := verifiedArtifact(keyDir, "recovery-journal.pub", m.Keys["recovery-journal.pub"], m.SHA256)
	if len(journalPub) != ed25519.PublicKeySize {
		return report, fmt.Errorf("management recovery: recovery-journal.pub is not an Ed25519 public key")
	}
	for _, binding := range []struct {
		required bool
		logical  string
	}{{m.ProviderKeysRequired, "provider-keys.json"}, {m.PolicyExemptionsRequired, "policy-exemptions.yaml"},
		{m.SharedUploadsRequired, "shared-uploads"}, {m.PrivateWorkspacesRequired, "private-workspaces"}} {
		if binding.required {
			if _, err := verifiedArtifact(setDir, binding.logical, m.Artifacts[binding.logical], m.SHA256); err != nil {
				return report, err
			}
		}
	}

	caCert, _ := verifiedArtifact(setDir, "ca.crt", m.Artifacts["ca.crt"], m.SHA256)
	caKey, _ := verifiedArtifact(setDir, "ca.key", m.Artifacts["ca.key"], m.SHA256)
	if err := verifyCA(caCert, caKey); err != nil {
		return report, err
	}
	opKey, _ := verifiedArtifact(setDir, "op-signing.key", m.Artifacts["op-signing.key"], m.SHA256)
	opPub, _ := verifiedArtifact(setDir, "op-signing.pub", m.Artifacts["op-signing.pub"], m.SHA256)
	if len(opKey) != ed25519.PrivateKeySize || len(opPub) != ed25519.PublicKeySize || !bytes.Equal(ed25519.PrivateKey(opKey).Public().(ed25519.PublicKey), opPub) {
		return report, fmt.Errorf("management recovery: operation-signing key pair does not match")
	}
	tokens, _ := verifiedArtifact(setDir, "tokens.json", m.Artifacts["tokens.json"], m.SHA256)
	if err := gateway.ValidatePersistedTokenStore(tokens); err != nil {
		return report, fmt.Errorf("management recovery: join-token state is invalid: %w", err)
	}

	work, err := os.MkdirTemp("", "wildstone-management-preflight-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(work)
	stateBundle := filepath.Join(setDir, filepath.FromSlash(m.Artifacts["statefile.bundle"]))
	restoredState, err := statefile.RestoreBundle(stateBundle, filepath.Join(work, "statefile"))
	if err != nil {
		return report, fmt.Errorf("management recovery: Statefile bundle: %w", err)
	}
	if !restoredState.HasCommit(m.ApprovedStatefileRevision) {
		return report, fmt.Errorf("management recovery: approved Statefile revision is missing")
	}
	dbPath := filepath.Join(setDir, filepath.FromSlash(m.Artifacts["keystone.db"]))
	keyCheckPath := filepath.Join(work, "key-check.db")
	if err := copyRegularFile(dbPath, keyCheckPath); err != nil {
		return report, fmt.Errorf("management recovery: copying runtime database for credential-key verification: %w", err)
	}
	keyCheckDB, err := runtimedb.Open(keyCheckPath)
	if err != nil {
		return report, fmt.Errorf("management recovery: opening runtime database for credential-key verification: %w", err)
	}
	householdKeyPath := filepath.Join(setDir, filepath.FromSlash(m.Artifacts["household-secrets.key"]))
	if _, err := keyCheckDB.OpenSecretVault(householdKeyPath); err != nil {
		keyCheckDB.Close()
		return report, fmt.Errorf("management recovery: household credential key does not match the runtime database: %w", err)
	}
	if err := keyCheckDB.Close(); err != nil {
		return report, fmt.Errorf("management recovery: closing credential-key verification database: %w", err)
	}
	restoredDB, err := runtimedb.RestoreBackup(dbPath, filepath.Join(work, "keystone.db"), "management recovery preflight", m.SnapshotAt)
	if err != nil {
		return report, fmt.Errorf("management recovery: runtime database: %w", err)
	}
	defer restoredDB.Close()
	state, err := restoredDB.GetRestoreState()
	if err != nil || !state.Quarantined {
		return report, fmt.Errorf("management recovery: restored runtime database did not enter quarantine")
	}
	head, err := restoredDB.StatefileHead()
	if err != nil || head != m.ApprovedStatefileRevision {
		return report, fmt.Errorf("management recovery: runtime database and approved Statefile revision disagree")
	}
	lease, err := restoredDB.GetCoordinator()
	if err != nil {
		return report, fmt.Errorf("management recovery: durable coordinator state is missing: %w", err)
	}
	for _, logical := range []string{"promotion.bundle", "delivery.bundle"} {
		bundle := filepath.Join(setDir, filepath.FromSlash(m.Artifacts[logical]))
		if err := verifyDeploymentBundle(bundle, filepath.Join(work, logical), m.PromotedDeploymentRevision); err != nil {
			return report, fmt.Errorf("management recovery: %s: %w", logical, err)
		}
	}
	return ManagementPreflight{SnapshotAt: m.SnapshotAt, ApprovedStatefileRevision: head,
		PromotedDeploymentRevision: m.PromotedDeploymentRevision,
		CoordinatorHolder:          lease.Holder, CoordinatorEpoch: lease.Epoch}, nil
}

// RestoreManagementSet installs the verified same-installation runtime group
// into a new, empty data directory. The database in that actual target is
// placed in restore quarantine before the directory becomes visible at its
// final name. The source recovery set is never modified.
func RestoreManagementSet(setDir, keyDir, dataDir string) (ManagementPreflight, error) {
	var report ManagementPreflight
	setRoot, err := filepath.EvalSymlinks(setDir)
	if err != nil {
		return report, fmt.Errorf("management recovery: resolving set directory: %w", err)
	}
	keyRoot, err := filepath.EvalSymlinks(keyDir)
	if err != nil {
		return report, fmt.Errorf("management recovery: resolving key directory: %w", err)
	}
	manifestPath := filepath.Join(setRoot, "management-manifest.json")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		return report, fmt.Errorf("management recovery: reading manifest for restore: %w", err)
	}
	report, err = VerifyManagementSet(setRoot, keyRoot)
	if err != nil {
		return report, err
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(manifestBefore, body) {
		return report, fmt.Errorf("management recovery: manifest changed during verification")
	}
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) == string(filepath.Separator) {
		return report, fmt.Errorf("management recovery: target data directory must be an absolute, non-root path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(dataDir)))
	if err != nil {
		return report, fmt.Errorf("management recovery: resolving target parent: %w", err)
	}
	target := filepath.Join(parent, filepath.Base(filepath.Clean(dataDir)))
	if _, err := os.Lstat(target); err == nil {
		return report, fmt.Errorf("management recovery: target data directory already exists")
	} else if !os.IsNotExist(err) {
		return report, fmt.Errorf("management recovery: checking target data directory: %w", err)
	}

	var manifest ManagementManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return report, fmt.Errorf("management recovery: decoding manifest for restore: %w", err)
	}

	stage, err := os.MkdirTemp(parent, ".wildstone-management-restore-")
	if err != nil {
		return report, fmt.Errorf("management recovery: creating target staging directory: %w", err)
	}
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return report, fmt.Errorf("management recovery: securing target staging directory: %w", err)
	}

	stateBundle := filepath.Join(setRoot, filepath.FromSlash(manifest.Artifacts["statefile.bundle"]))
	if _, err := statefile.RestoreBundle(stateBundle, filepath.Join(stage, "statefile")); err != nil {
		return report, fmt.Errorf("management recovery: restoring Statefile: %w", err)
	}
	dbSource := filepath.Join(setRoot, filepath.FromSlash(manifest.Artifacts["keystone.db"]))
	db, err := runtimedb.RestoreBackup(dbSource, filepath.Join(stage, "keystone.db"), "management-independent recovery", manifest.SnapshotAt)
	if err != nil {
		return report, fmt.Errorf("management recovery: restoring runtime database: %w", err)
	}
	restoreState, stateErr := db.GetRestoreState()
	closeErr := db.Close()
	if stateErr != nil || !restoreState.Quarantined {
		return report, fmt.Errorf("management recovery: target runtime database did not enter quarantine")
	}
	if closeErr != nil {
		return report, fmt.Errorf("management recovery: closing target runtime database: %w", closeErr)
	}

	runtimeFiles := []string{"ca.crt", "ca.key", "op-signing.key", "op-signing.pub", "tokens.json", "household-secrets.key"}
	if manifest.ProviderKeysRequired {
		runtimeFiles = append(runtimeFiles, "provider-keys.json")
	}
	if manifest.PolicyExemptionsRequired {
		runtimeFiles = append(runtimeFiles, "policy-exemptions.yaml")
	}
	for _, logical := range runtimeFiles {
		source := filepath.Join(setRoot, filepath.FromSlash(manifest.Artifacts[logical]))
		if err := copyRegularFile(source, filepath.Join(stage, logical)); err != nil {
			return report, fmt.Errorf("management recovery: restoring %s: %w", logical, err)
		}
	}
	journalPubSource := filepath.Join(keyRoot, filepath.FromSlash(manifest.Keys["recovery-journal.pub"]))
	if err := copyRegularFile(journalPubSource, filepath.Join(stage, "recovery-journal.pub")); err != nil {
		return report, fmt.Errorf("management recovery: restoring independently held journal trust root: %w", err)
	}
	if err := os.Rename(stage, target); err != nil {
		return report, fmt.Errorf("management recovery: publishing target data directory: %w", err)
	}
	removeStage = false
	if dir, err := os.Open(parent); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return report, nil
}

func copyRegularFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func verifiedArtifact(root, logical, rel string, sums map[string]string) ([]byte, error) {
	if root == "" || rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) || filepath.Clean(filepath.FromSlash(rel)) != filepath.FromSlash(rel) {
		return nil, fmt.Errorf("management recovery: %s is missing or has an unsafe path", logical)
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !withinPath(root, real) {
		return nil, fmt.Errorf("management recovery: %s is missing or escapes its material directory", logical)
	}
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("management recovery: %s is missing or not a regular file", logical)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("management recovery: reading %s: %w", logical, err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("management recovery: %s is empty", logical)
	}
	sum := sha256Hex(body)
	if sums[logical] == "" || sums[logical] != sum {
		return nil, fmt.Errorf("management recovery: %s failed integrity check", logical)
	}
	return body, nil
}

func pathsOverlap(a, b string) bool {
	return withinPath(a, b) || withinPath(b, a)
}

func withinPath(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && (rel == "." || (filepath.IsLocal(rel) && rel != ".."))
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func verifyCA(certPEM, keyPEM []byte) error {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || cb.Type != "CERTIFICATE" || kb == nil {
		return fmt.Errorf("management recovery: CA certificate/key encoding is invalid")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil || !cert.IsCA {
		return fmt.Errorf("management recovery: CA certificate is invalid")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if err != nil || !ok || !pub.Equal(&key.PublicKey) {
		return fmt.Errorf("management recovery: CA certificate/key pair does not match")
	}
	return nil
}

func verifyDeploymentBundle(bundle, destination, revision string) error {
	cmd := exec.Command("git", "clone", "--bare", "--no-local", bundle, destination)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cloning bundle: %w: %s", err, bytes.TrimSpace(out))
	}
	cmd = exec.Command("git", "--git-dir="+destination, "rev-parse", "--verify", "refs/heads/deploy^{commit}")
	out, err := cmd.Output()
	if err != nil || string(bytes.TrimSpace(out)) != revision {
		return fmt.Errorf("promoted deploy revision is missing")
	}
	return nil
}
