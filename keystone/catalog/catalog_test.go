package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func writePkg(t *testing.T, root, dir, name, version, capability string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: wildstone.dev/v1\nkind: Package\nmetadata:\n  name: " + name +
		"\n  version: " + version + "\n  capability: " + capability +
		"\nprovides:\n  - { trait: " + capability + ", mode: oidc }\n" +
		"params:\n  - { name: node, required: true }\n" +
		"templates:\n  - |\n    apiVersion: wildstone.dev/v1\n    kind: Volume\n" +
		"    metadata: { name: v, node: \"{{ .Params.node }}\" }\n    spec: { name: v }\n"
	if err := os.WriteFile(filepath.Join(d, "package.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadDirSkipsNonPackagesButNotBrokenOnes states the asymmetry. A
// catalog directory is allowed to hold a README; it is not allowed to
// hold a package.yaml that does not parse. Skipping the second would
// make "the package is not in the catalog" the symptom of a typo, which
// is a much worse thing to debug than a refusal to start.
func TestLoadDirSkipsNonPackagesButNotBrokenOnes(t *testing.T) {
	root := t.TempDir()
	writePkg(t, root, "identity", "identity", "1.0.0", "identity")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadDir(root)
	if err != nil {
		t.Fatalf("a directory without a package.yaml should be skipped, not fatal: %v", err)
	}
	if len(c.List()) != 1 {
		t.Fatalf("want 1 package, got %d", len(c.List()))
	}

	if err := os.WriteFile(filepath.Join(root, "docs", "package.yaml"), []byte("{{ not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(root); err == nil {
		t.Error("a package.yaml that does not parse must fail the load, not be skipped")
	}
}

// TestGetRefusesToGuessAVersion: an empty version means "the only one".
// Picking for the operator when several exist is not a service worth
// providing — it is the wrong install, silently.
func TestGetRefusesToGuessAVersion(t *testing.T) {
	root := t.TempDir()
	writePkg(t, root, "a", "photos-immich", "1.0.0", "photos")
	writePkg(t, root, "b", "photos-immich", "2.0.0", "photos")
	c, err := LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("photos-immich", ""); err == nil {
		t.Error("want a refusal when two versions exist and none was named")
	}
	got, err := c.Get("photos-immich", "2.0.0")
	if err != nil || got.Metadata.Version != "2.0.0" {
		t.Errorf("naming a version should resolve it: %v", err)
	}
	if _, err := c.Get("nope", ""); err == nil {
		t.Error("want an error for a package that is not in the catalog")
	}
}

// TestProvidersForUsesEnabledCapabilities is ADR-W021 A4, which closes
// F40: `requires` resolves against what the deployment has enabled, not
// against directories passed on a command line.
func TestProvidersForUsesEnabledCapabilities(t *testing.T) {
	root := t.TempDir()
	writePkg(t, root, "identity", "identity", "1.0.0", "identity")
	writePkg(t, root, "media", "media-jellyfin", "1.0.0", "media")
	c, err := LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	got := c.ProvidersFor(map[string]bool{"identity": true})
	if len(got) != 1 || got[0].Metadata.Capability != "identity" {
		t.Fatalf("want only the enabled provider, got %d", len(got))
	}
	if len(c.ProvidersFor(nil)) != 0 {
		t.Error("nothing is enabled, so nothing provides")
	}
}
