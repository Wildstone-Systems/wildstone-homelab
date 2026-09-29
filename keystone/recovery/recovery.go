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
	"path/filepath"
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
	Format      string            `json:"format"`
	SnapshotAt  time.Time         `json:"snapshotAt"`
	Nodes       []string          `json:"nodes"`
	NodeBundles map[string]string `json:"nodeBundles"`
	SHA256      map[string]string `json:"sha256"`
}

// Prepare writes a complete set into a sibling staging directory, then
// promotes it atomically. The stable directory is the sole source path of
// BackupPlan wildstone, so restic can never observe a half-written set.
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
	absSource, err := filepath.Abs(plan.BackupPlan.Paths[0])
	if err != nil || filepath.Clean(absSource) != filepath.Clean(absDir) {
		return fmt.Errorf("recovery: BackupPlan %q source must be %s", PlanName, absDir)
	}
	if err := os.MkdirAll(filepath.Dir(absDir), 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(absDir), ".wildstone-recovery-next-")
	if err != nil {
		return fmt.Errorf("recovery: creating stage: %w", err)
	}
	defer os.RemoveAll(stage)

	manifest := Manifest{Format: "wildstone-recovery-set-v1", SnapshotAt: b.now().UTC(), NodeBundles: map[string]string{}, SHA256: map[string]string{}}
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

	nodes, err := b.DB.ListNodes()
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if node.ProtocolMinor < RequiredProtocolMin {
			return fmt.Errorf("recovery: node %s uses protocol minor %d; %d is required", node.NodeID, node.ProtocolMinor, RequiredProtocolMin)
		}
		res, err := b.Agents.Apply(ctx, node.NodeID, &wsv1.Operation{Op: &wsv1.Operation_ExportSecretBundle{
			ExportSecretBundle: &wsv1.ExportSecretBundle{RecoveryPublicKey: plan.BackupPlan.RecoveryPublicKey},
		}})
		if err != nil {
			return fmt.Errorf("recovery: exporting node %s secrets: %w", node.NodeID, err)
		}
		if oerr := res.GetError(); oerr != nil {
			return fmt.Errorf("recovery: exporting node %s secrets: %s", node.NodeID, oerr.GetMessage())
		}
		bundle := res.GetExportSecretBundle()
		if bundle == nil {
			return fmt.Errorf("recovery: node %s returned no sealed bundle", node.NodeID)
		}
		name := nodeBundleName(node.NodeID)
		if err := os.WriteFile(filepath.Join(stage, name), bundle.GetSealedBundle(), 0o600); err != nil {
			return fmt.Errorf("recovery: writing node %s bundle: %w", node.NodeID, err)
		}
		manifest.Nodes = append(manifest.Nodes, node.NodeID)
		manifest.NodeBundles[node.NodeID] = name
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
	return promote(stage, absDir)
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
