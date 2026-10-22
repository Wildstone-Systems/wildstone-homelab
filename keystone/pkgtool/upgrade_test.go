package pkgtool

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/statefile"
)

// TestUpgradeIsAnOrdinaryPlan pins ADR-W010 A4 end to end: publish v1,
// deploy its expansion into a Statefile, publish v2 (a new image digest),
// resolve ^1 → v2 from the registry, verify + extract + expand it, and
// diff against the deployed state — the resulting plan IS the upgrade
// plan, one Update step, no special mechanism anywhere.
func TestUpgradeIsAnOrdinaryPlan(t *testing.T) {
	const digestV1 = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	const digestV2 = "sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	pkgYAML := func(version, digest string) string {
		return `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: app, version: ` + version + `, capability: app, profile: default }
params:
  - { name: node, required: true }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: app, node: "{{ .Params.node }}" }
    spec:
      image: "reg.example/app@` + digest + `"
`
	}

	registry := t.TempDir()
	_, priv, err := GenerateSigningKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	v1dir := writePackage(t, pkgYAML("1.0.0", digestV1))
	if _, err := Publish(registry, v1dir, priv); err != nil {
		t.Fatal(err)
	}

	// Deploy v1's expansion into a Statefile — the running desired state.
	params := map[string]string{"node": "n1"}
	v1pkg, err := pkgspec.Load(filepath.Join(v1dir, "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	engine := capability.NewEngine()
	v1resources, err := engine.Expand(v1pkg, params, capability.NewCatalog(v1pkg))
	if err != nil {
		t.Fatal(err)
	}
	state, err := statefile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range v1resources {
		if err := state.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Commit("test", "plan-1", "deploy app 1.0.0"); err != nil {
		t.Fatal(err)
	}

	// Publish v1.1.0 with a new image digest.
	v2dir := writePackage(t, pkgYAML("1.1.0", digestV2))
	if _, err := Publish(registry, v2dir, priv); err != nil {
		t.Fatal(err)
	}

	// Resolve ^1 → 1.1.0; verify the archive; extract; expand.
	idx, err := ReadIndex(registry)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := idx.Resolve("app", "^1")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Version != "1.1.0" {
		t.Fatalf("resolved %s, want 1.1.0", entry.Version)
	}
	archive, err := os.ReadFile(filepath.Join(registry, entry.Path))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	if err := VerifyEntry(entry, hex.EncodeToString(sum[:]), pub); err != nil {
		t.Fatalf("VerifyEntry: %v", err)
	}
	extracted := t.TempDir()
	if err := ExtractArchive(bytes.NewReader(archive), extracted); err != nil {
		t.Fatal(err)
	}
	v2pkg, err := pkgspec.Load(filepath.Join(extracted, "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v2resources, err := engine.Expand(v2pkg, params, capability.NewCatalog(v2pkg))
	if err != nil {
		t.Fatal(err)
	}

	// The diff against deployed state IS the upgrade plan.
	current, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Diff(v2resources, current)
	if len(p.Steps) != 1 || p.Steps[0].Action != plan.ActionUpdate || p.Steps[0].Name != "app" {
		t.Fatalf("upgrade plan = %+v, want exactly one Update of Container/app", p.Steps)
	}
	if !strings.Contains(p.Steps[0].BlastRadius, "service interruption") {
		t.Fatalf("upgrade step carries no blast-radius summary: %+v", p.Steps[0])
	}
}
