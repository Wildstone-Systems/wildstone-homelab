package pkgtool

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoPackages points at the real shipped packages — linting them here
// makes the toolchain the standing contract test for packages/ itself.
func repoPackages(t *testing.T) (identity, photos string) {
	t.Helper()
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "identity"), filepath.Join(root, "photos-immich")
}

// TestLintShippedPackages walks ALL of packages/ (ADR-W011 B/I1): a new
// package cannot ship unlinted, because merely adding its directory puts
// it under this test. Each package lints with every other shipped package
// on the catalog, and shared real-shaped fillers cover the params whose
// value shape matters (domains); everything else auto-fills.
func TestLintShippedPackages(t *testing.T) {
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	if len(dirs) < 6 {
		t.Fatalf("found only %d package dirs under %s — packages/ walk is broken", len(dirs), root)
	}

	fillers := map[string]string{
		"public_domain": "example.internal",
		"admin_domain":  "admin.example.internal",
		"node":          "n1",
	}
	for _, dir := range dirs {
		dir := dir
		t.Run(filepath.Base(dir), func(t *testing.T) {
			var catalog []string
			for _, other := range dirs {
				if other != dir {
					catalog = append(catalog, other)
				}
			}
			issues, err := Lint(dir, LintOptions{Fillers: fillers, CatalogDirs: catalog})
			if err != nil {
				t.Fatalf("Lint(%s): %v", filepath.Base(dir), err)
			}
			if len(issues) != 0 {
				t.Fatalf("shipped package %s has lint issues: %v", filepath.Base(dir), issues)
			}
		})
	}
}

func writePackage(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const badRoutePackage = `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: badpkg, version: 1.0.0, capability: badpkg, profile: default }
params:
  - { name: node, required: true }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Route
    metadata: { name: badpkg-route, node: "{{ .Params.node }}" }
    spec:
      host: bad.example.com
      upstream: 10.0.0.1:80
      auth: none
      classification: family_public
`

func TestLintCatchesPolicyViolation(t *testing.T) {
	dir := writePackage(t, badRoutePackage)
	issues, err := Lint(dir, LintOptions{})
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	found := false
	for _, i := range issues {
		if i.Rule == "policy" && strings.Contains(i.Msg, "public-requires-auth") {
			found = true
		}
	}
	if !found {
		t.Fatalf("policy-violating package passed lint: %v", issues)
	}
}

func TestLintUnsatisfiedTraitAndStructural(t *testing.T) {
	pkg := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: needy, version: 1.0.0, capability: needy, profile: default }
requires:
  - { trait: ingress, mode: public }
  - { trait: identity, mode: oidc }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: needy-data, node: n1 }
    spec: { name: needy-data }
`
	dir := writePackage(t, pkg)
	// identity:oidc unsatisfied → expand issue; ingress:public must NOT
	// be the complaint (structural, ADR-W010 A1).
	issues, err := Lint(dir, LintOptions{})
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(issues) != 1 || issues[0].Rule != "expand" ||
		!strings.Contains(issues[0].Msg, "identity") || strings.Contains(issues[0].Msg, "ingress") {
		t.Fatalf("issues = %v, want one expand issue about identity only", issues)
	}
	// With the real identity package on the catalog: clean.
	identity, _ := repoPackages(t)
	issues, err = Lint(dir, LintOptions{CatalogDirs: []string{identity}})
	if err != nil || len(issues) != 0 {
		t.Fatalf("with catalog: issues = %v, err %v", issues, err)
	}
}

func TestArchiveDeterministicAndRoundTrips(t *testing.T) {
	dir := writePackage(t, badRoutePackage) // content irrelevant here
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "notes.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var a, b bytes.Buffer
	d1, err := BuildArchive(dir, &a)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := BuildArchive(dir, &b)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("archive is not deterministic across builds")
	}
	sum := sha256.Sum256(a.Bytes())
	if hex.EncodeToString(sum[:]) != d1 {
		t.Fatal("returned digest is not the sha256 of the written bytes")
	}

	out := t.TempDir()
	if err := ExtractArchive(&a, out); err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "sub", "notes.md"))
	if err != nil || string(got) != "hi\n" {
		t.Fatalf("extracted content = %q, %v", got, err)
	}
}

func TestSignVerifyAndTamper(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "publisher.key")
	pub, priv, err := GenerateSigningKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Keys round-trip through their files.
	loadedPriv, err := LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	loadedPub, err := LoadPublicKey(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loadedPriv, priv) || !bytes.Equal(loadedPub, pub) {
		t.Fatal("key files do not round-trip")
	}

	digest := strings.Repeat("ab", 32)
	sig := SignArchive(priv, digest)
	if !VerifyArchive(pub, digest, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyArchive(pub, strings.Repeat("cd", 32), sig) {
		t.Fatal("signature verified against a different digest")
	}
	sig[0] ^= 1
	if VerifyArchive(pub, digest, sig) {
		t.Fatal("tampered signature verified")
	}
}

func TestPublishResolveVerify(t *testing.T) {
	identity, _ := repoPackages(t)
	root := t.TempDir()
	pub, priv, err := GenerateSigningKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}

	entry, err := Publish(root, identity, priv)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if entry.Name != "identity" || entry.Version != "1.0.0" {
		t.Fatalf("entry = %+v", entry)
	}

	idx, err := ReadIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := idx.Resolve("identity", "")
	if err != nil || got.Sha256 != entry.Sha256 {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	if _, err := idx.Resolve("identity", "^2"); err == nil {
		t.Fatal("Resolve(^2) should fail: no 2.x published")
	}
	if _, err := idx.Resolve("ghost", ""); err == nil {
		t.Fatal("Resolve of unpublished package should fail")
	}

	// The stored archive verifies against its entry with the publisher key…
	data, err := os.ReadFile(filepath.Join(root, entry.Path))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := VerifyEntry(entry, hex.EncodeToString(sum[:]), pub); err != nil {
		t.Fatalf("VerifyEntry: %v", err)
	}

	// …and a bit-flipped archive does not.
	data[100] ^= 1
	sum = sha256.Sum256(data)
	if err := VerifyEntry(entry, hex.EncodeToString(sum[:]), pub); err == nil {
		t.Fatal("tampered archive verified")
	}

	// Republishing the same version replaces, not duplicates.
	if _, err := Publish(root, identity, priv); err != nil {
		t.Fatal(err)
	}
	idx, _ = ReadIndex(root)
	if len(idx.Packages) != 1 {
		t.Fatalf("index has %d entries after republish, want 1", len(idx.Packages))
	}
}

func TestSemver(t *testing.T) {
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "v1.2.3", "1.02.3", "1.2.3-rc1"} {
		if _, err := parseSemver(bad); err == nil {
			t.Errorf("parseSemver(%q) accepted", bad)
		}
	}
	a, _ := parseSemver("1.9.0")
	b, _ := parseSemver("1.10.0")
	if !a.less(b) || b.less(a) {
		t.Fatal("1.9.0 must sort below 1.10.0 (numeric, not lexical)")
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/abs/evil", "a/../../evil"} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()
		if err := ExtractArchive(&buf, t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("hostile member %q: err = %v, want escape rejection", name, err)
		}
	}

	// A symlink member is refused outright (only regular files exist in
	// the format).
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if err := ExtractArchive(&buf, t.TempDir()); err == nil || !strings.Contains(err.Error(), "regular files") {
		t.Fatalf("symlink member: err = %v, want type rejection", err)
	}
}
