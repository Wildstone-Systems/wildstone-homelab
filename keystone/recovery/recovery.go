// Package recovery builds the ADR-W045 A5 whole-site recovery set.
package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

const (
	PlanName            = "wildstone"
	RequiredProtocolMin = 20
)

type Builder struct {
	State  *statefile.Store
	DB     *runtimedb.DB
	Vault  *runtimedb.SecretVault
	Agents reconcile.Applier
	Dir    string
	Now    func() time.Time
}

type Manifest struct {
	Format                  string            `json:"format"`
	SnapshotAt              time.Time         `json:"snapshotAt"`
	RecoveryPublicKeySHA256 string            `json:"recoveryPublicKeySHA256"`
	Nodes                   []string          `json:"nodes"`
	NodeBundles             map[string]string `json:"nodeBundles"`
	SHA256                  map[string]string `json:"sha256"`
}

// Prepare writes a complete set into a local sibling staging directory,
// promotes it atomically, then installs its files on the selected Wardstone
// node with manifest.json last. The plan's source path is node-local; Keystone
// may run on another host and must never assume the same absolute path names
// the same filesystem there.
func (b *Builder) Prepare(ctx context.Context, plan resource.Resource) error {
	if plan.Metadata.Name != PlanName || plan.BackupPlan == nil {
		return fmt.Errorf("recovery: only BackupPlan %q can build a whole-site set", PlanName)
	}
	if err := recoveryseal.ValidatePublicKey(plan.BackupPlan.RecoveryPublicKey); err != nil {
		return fmt.Errorf("recovery: invalid public key")
	}
	if b.State == nil || b.DB == nil || b.Vault == nil || b.Agents == nil || b.Dir == "" {
		return fmt.Errorf("recovery: builder is not fully configured")
	}
	absDir, err := filepath.Abs(b.Dir)
	if err != nil {
		return fmt.Errorf("recovery: resolving staging directory: %w", err)
	}
	if len(plan.BackupPlan.Paths) != 1 {
		return fmt.Errorf("recovery: BackupPlan %q must have exactly one path", PlanName)
	}
	if filepath.Clean(plan.BackupPlan.Paths[0]) != filepath.Clean(resource.WholeSiteRecoveryPath) {
		return fmt.Errorf("recovery: BackupPlan %q source must be %s", PlanName, resource.WholeSiteRecoveryPath)
	}
	if err := os.MkdirAll(filepath.Dir(absDir), 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(absDir), ".wildstone-recovery-next-")
	if err != nil {
		return fmt.Errorf("recovery: creating stage: %w", err)
	}
	defer os.RemoveAll(stage)

	keySum := sha256.Sum256([]byte(plan.BackupPlan.RecoveryPublicKey))
	manifest := Manifest{
		Format: "wildstone-recovery-set-v1", SnapshotAt: b.now().UTC(),
		RecoveryPublicKeySHA256: hex.EncodeToString(keySum[:]),
		NodeBundles:             map[string]string{}, SHA256: map[string]string{},
	}
	if err := b.State.Bundle(filepath.Join(stage, "statefile.bundle")); err != nil {
		return err
	}
	if err := b.DB.BackupTo(filepath.Join(stage, "keystone.db")); err != nil {
		return err
	}
	household, err := b.Vault.ExportRecoveryBundle(plan.BackupPlan.RecoveryPublicKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "household-secrets.sealed"), household, 0o600); err != nil {
		return fmt.Errorf("recovery: writing household bundle: %w", err)
	}

	desired, err := b.State.Load()
	if err != nil {
		return err
	}
	active := map[string]bool{}
	for _, r := range desired {
		if r.Metadata.Node != "" {
			active[r.Metadata.Node] = true
		}
	}
	activeNodes := make([]string, 0, len(active))
	for node := range active {
		activeNodes = append(activeNodes, node)
	}
	sort.Strings(activeNodes)
	records, err := b.DB.ListNodes()
	if err != nil {
		return err
	}
	byID := make(map[string]runtimedb.Node, len(records))
	for _, node := range records {
		byID[node.NodeID] = node
	}
	previous := reusableNodeBundles(absDir, manifest.RecoveryPublicKeySHA256)
	for _, nodeID := range activeNodes {
		name := nodeBundleName(nodeID)
		sealed, exportErr := b.exportNodeBundle(ctx, nodeID, byID[nodeID], plan.BackupPlan.RecoveryPublicKey)
		if exportErr != nil {
			sealed = previous[nodeID]
			if len(sealed) == 0 {
				return exportErr
			}
		}
		if err := os.WriteFile(filepath.Join(stage, name), sealed, 0o600); err != nil {
			return fmt.Errorf("recovery: writing node %s bundle: %w", nodeID, err)
		}
		manifest.Nodes = append(manifest.Nodes, nodeID)
		manifest.NodeBundles[nodeID] = name
	}
	files := append([]string{"statefile.bundle", "keystone.db", "household-secrets.sealed"}, nodeBundleNames(manifest.Nodes)...)
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(stage, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		manifest.SHA256[name] = hex.EncodeToString(sum[:])
	}
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(manifestBody, '\n'), 0o600); err != nil {
		return err
	}
	if err := promote(stage, absDir); err != nil {
		return err
	}
	return b.installOnNode(ctx, plan.Metadata.Node, plan.BackupPlan.Paths[0], absDir, append(files, "manifest.json"))
}

func (b *Builder) exportNodeBundle(ctx context.Context, nodeID string, node runtimedb.Node, publicKey string) ([]byte, error) {
	if node.NodeID == "" {
		return nil, fmt.Errorf("recovery: active node %s has no join record", nodeID)
	}
	if node.ProtocolMinor < RequiredProtocolMin {
		return nil, fmt.Errorf("recovery: node %s uses protocol minor %d; %d is required", nodeID, node.ProtocolMinor, RequiredProtocolMin)
	}
	res, err := b.Agents.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_ExportSecretBundle{
		ExportSecretBundle: &wsv1.ExportSecretBundle{RecoveryPublicKey: publicKey},
	}})
	if err != nil {
		return nil, fmt.Errorf("recovery: exporting node %s secrets: %w", nodeID, err)
	}
	if oerr := res.GetError(); oerr != nil {
		return nil, fmt.Errorf("recovery: exporting node %s secrets: %s", nodeID, oerr.GetMessage())
	}
	bundle := res.GetExportSecretBundle()
	if bundle == nil {
		return nil, fmt.Errorf("recovery: node %s returned no sealed bundle", nodeID)
	}
	return bundle.GetSealedBundle(), nil
}

// reusableNodeBundles returns sealed bundles only when the previous complete
// set used the same recovery public key. Reusing a bundle encrypted to an old
// key would create a backup that verifies but cannot be recovered.
func reusableNodeBundles(dir, publicKeySHA256 string) map[string][]byte {
	out := map[string][]byte{}
	manifest, err := VerifySet(dir)
	if err != nil || manifest.RecoveryPublicKeySHA256 != publicKeySHA256 {
		return out
	}
	for _, node := range manifest.Nodes {
		if body, err := os.ReadFile(filepath.Join(dir, manifest.NodeBundles[node])); err == nil {
			out[node] = body
		}
	}
	return out
}

// installOnNode copies the completed set through Wardstone. Every artifact is
// an atomic managed-file write and the manifest is deliberately last, so the
// reserved restic run that follows can only select a fully transferred set.
func (b *Builder) installOnNode(ctx context.Context, node, destination, source string, files []string) error {
	if node == "" {
		return fmt.Errorf("recovery: BackupPlan %q requires metadata.node", PlanName)
	}
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			return fmt.Errorf("recovery: reading staged artifact %s: %w", name, err)
		}
		res, err := b.Agents.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_WriteManagedFile{
			WriteManagedFile: &wsv1.WriteManagedFile{Path: path.Join(destination, name), Content: body, Mode: 0o600},
		}})
		if err != nil {
			return fmt.Errorf("recovery: staging artifact %s on node %s: %w", name, node, err)
		}
		if oerr := res.GetError(); oerr != nil {
			return fmt.Errorf("recovery: staging artifact %s on node %s: %s", name, node, oerr.GetMessage())
		}
	}
	return nil
}

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func nodeBundleNames(nodes []string) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = nodeBundleName(node)
	}
	return names
}

func nodeBundleName(node string) string {
	return "node-" + base64.RawURLEncoding.EncodeToString([]byte(node)) + ".sealed"
}

func promote(stage, destination string) error {
	previous := destination + ".previous"
	if _, err := os.Stat(destination); os.IsNotExist(err) {
		if _, previousErr := os.Stat(previous); previousErr == nil {
			if err := os.Rename(previous, destination); err != nil {
				return fmt.Errorf("recovery: recovering interrupted promotion: %w", err)
			}
		}
	}
	if err := os.RemoveAll(previous); err != nil {
		return fmt.Errorf("recovery: removing old recovery stage: %w", err)
	}
	hadCurrent := false
	if _, err := os.Stat(destination); err == nil {
		if err := os.Rename(destination, previous); err != nil {
			return fmt.Errorf("recovery: preserving current set: %w", err)
		}
		hadCurrent = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		if hadCurrent {
			_ = os.Rename(previous, destination)
		}
		return fmt.Errorf("recovery: promoting complete set: %w", err)
	}
	if err := os.RemoveAll(previous); err != nil {
		return fmt.Errorf("recovery: removing previous set: %w", err)
	}
	return nil
}
