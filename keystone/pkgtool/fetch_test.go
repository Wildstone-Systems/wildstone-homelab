package pkgtool

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchPackageVerifiedOverHTTP pins ADR-W010 A3's escape hatch: a
// registry directory served by a plain static file server is a complete
// registry, and the fetch path verifies digest + signature before
// anything is extracted.
func TestFetchPackageVerifiedOverHTTP(t *testing.T) {
	identity, _ := repoPackages(t)
	root := t.TempDir()
	pub, priv, err := GenerateSigningKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(root, identity, priv); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)

	dest, err := FetchPackage(srv.URL, "identity", "", pub, t.TempDir())
	if err != nil {
		t.Fatalf("FetchPackage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "package.yaml")); err != nil {
		t.Fatalf("fetched package missing package.yaml: %v", err)
	}
	// The fetched package lints clean — the full trust path end to end.
	issues, err := Lint(dest, LintOptions{})
	if err != nil || len(issues) != 0 {
		t.Fatalf("fetched package lint = %v, %v", issues, err)
	}

	// The wrong public key refuses the archive.
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FetchPackage(srv.URL, "identity", "", otherPub, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "signature") {
		t.Fatalf("fetch with wrong key = %v, want signature refusal", err)
	}

	// A tampered archive on the server is refused by digest.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tgz") {
			p := filepath.Join(root, e.Name())
			data, _ := os.ReadFile(p)
			data[50] ^= 1
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := FetchPackage(srv.URL, "identity", "", pub, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "digest") {
		t.Fatalf("fetch of tampered archive = %v, want digest refusal", err)
	}
}

func TestRefreshDigests(t *testing.T) {
	const oldDigest = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	const newDigest = "sha256:" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	dir := writePackage(t, `# a comment that must survive
apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: app, version: 1.0.0, capability: app, profile: default }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: app, node: n1 }
    spec:
      image: "ghcr.io/example/app@`+oldDigest+`"
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: cache, node: n1 }
    spec:
      image: "redis:7@`+oldDigest+`"
`)

	resolved := map[string]string{}
	resolver := func(image string) (string, error) {
		resolved[image] = image
		if image == "ghcr.io/example/app" {
			return newDigest, nil
		}
		return oldDigest, nil // redis already current
	}
	results, err := RefreshDigests(dir, resolver)
	if err != nil {
		t.Fatalf("RefreshDigests: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "ghcr.io/example/app@"+newDigest) {
		t.Fatalf("stale digest not rewritten:\n%s", text)
	}
	if !strings.Contains(text, "redis:7@"+oldDigest) {
		t.Fatalf("current digest was disturbed:\n%s", text)
	}
	if !strings.Contains(text, "# a comment that must survive") {
		t.Fatal("textual rewrite destroyed comments")
	}
	if _, ok := resolved["ghcr.io/example/app"]; !ok {
		t.Fatalf("resolver saw %v", resolved)
	}
}

func TestSplitImageRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"ghcr.io/immich-app/immich-server": {"ghcr.io", "immich-app/immich-server"},
		"redis":                            {"registry-1.docker.io", "library/redis"},
		"lldap/lldap":                      {"registry-1.docker.io", "lldap/lldap"},
	} {
		reg, repo := splitImageRef(ref)
		if reg != want[0] || repo != want[1] {
			t.Errorf("splitImageRef(%q) = %q,%q want %q,%q", ref, reg, repo, want[0], want[1])
		}
	}
}
