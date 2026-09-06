package pkgtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The trait vocabulary check (ADR-W025 A1). The failure it replaces is
// the quiet one: `requires: {trait: identiy}` was not a malformed
// package, merely one nothing satisfied, and it surfaced at install time
// as "unsatisfied requirement identiy:oidc" — a message that names the
// typo without saying it is one, in front of an operator who did not
// write the package.

// writeTraitPkg writes a minimal package whose only interesting content
// is its provides/requires/exports, given as raw YAML.
func writeTraitPkg(t *testing.T, blocks string) string {
	t.Helper()
	dir := t.TempDir()
	body := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: app, version: 1.0.0, capability: app }
` + blocks + `params:
  - { name: node, required: true }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: data, node: "{{ .Params.node }}" }
    spec: { name: data }
`
	if err := os.WriteFile(filepath.Join(dir, "package.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// issuesOfRule returns the messages of the issues under one rule, so a
// test asserts on the finding it is about rather than on lint's whole
// output.
func issuesOfRule(t *testing.T, dir, rule string) []string {
	t.Helper()
	issues, err := Lint(dir, LintOptions{})
	if err != nil {
		t.Fatalf("lint could not run: %v", err)
	}
	var out []string
	for _, i := range issues {
		if i.Rule == rule {
			out = append(out, i.Msg)
		}
	}
	return out
}

func TestLintRejectsAnUnknownTrait(t *testing.T) {
	dir := writeTraitPkg(t, "requires:\n  - { trait: identiy, mode: oidc }\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 {
		t.Fatalf("want one trait issue, got %v", got)
	}
	// The message must list what does exist, or the author is only told
	// they are wrong and not how to be right.
	for _, want := range []string{`"identiy"`, "identity", "keystone/trait"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("message does not mention %s: %s", want, got[0])
		}
	}
}

func TestLintRejectsAnUnknownMode(t *testing.T) {
	dir := writeTraitPkg(t, "requires:\n  - { trait: identity, mode: saml }\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 || !strings.Contains(got[0], "no mode \"saml\"") {
		t.Fatalf("want a mode issue naming saml, got %v", got)
	}
	if !strings.Contains(got[0], "oidc") {
		t.Errorf("message should list the modes that do exist: %s", got[0])
	}
}

func TestLintRejectsAnUnknownClass(t *testing.T) {
	dir := writeTraitPkg(t, "requires:\n  - { trait: storage, class: platinum }\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 || !strings.Contains(got[0], "no class \"platinum\"") {
		t.Fatalf("want a class issue naming platinum, got %v", got)
	}
}

// A mode on a trait that has none is its own mistake: storage takes a
// class, and a package saying `mode: bulk` has quietly asked for nothing.
func TestLintRejectsAModeOnATraitThatHasNone(t *testing.T) {
	dir := writeTraitPkg(t, "requires:\n  - { trait: storage, mode: bulk }\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 || !strings.Contains(got[0], "no modes") {
		t.Fatalf("want a mode issue saying storage has none, got %v", got)
	}
}

// The export half. A key the trait does not carry is the invisible
// failure: nothing errors, the value is simply never seen, and the
// consumer fails on a missing key with nothing connecting the two.
func TestLintRejectsAnExportKeyTheTraitDoesNotCarry(t *testing.T) {
	dir := writeTraitPkg(t, "provides:\n  - trait: identity\n    mode: oidc\n    exports:\n      PortalURL: \"https://example.com\"\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 || !strings.Contains(got[0], `no key "PortalURL"`) {
		t.Fatalf("want an export issue naming PortalURL, got %v", got)
	}
	if !strings.Contains(got[0], "IssuerURL") {
		t.Errorf("message should list the keys the mode does carry: %s", got[0])
	}
}

// An author reaching for an intrinsic key has made a specific mistake and
// deserves the specific answer: they already have it, and no export can
// replace one.
func TestLintExplainsAnIntrinsicKeyExport(t *testing.T) {
	dir := writeTraitPkg(t, "provides:\n  - trait: identity\n    mode: oidc\n    exports:\n      TailnetHost: \"node.example.ts.net\"\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 || !strings.Contains(got[0], "intrinsic") {
		t.Fatalf("want an issue explaining the key is intrinsic, got %v", got)
	}
}

// An export key that exists on ANOTHER trait is a near miss worth naming,
// because the fix is usually to change the trait rather than the key.
func TestLintPointsAtTheTraitThatCarriesAMisplacedKey(t *testing.T) {
	dir := writeTraitPkg(t, "provides:\n  - trait: identity\n    mode: ldap\n    exports:\n      IssuerURL: \"https://example.com\"\n")
	got := issuesOfRule(t, dir, "trait")
	if len(got) != 1 || !strings.Contains(got[0], "carried by identity:oidc") {
		t.Fatalf("want an issue pointing at identity:oidc, got %v", got)
	}
}

// The shipped vocabulary must lint clean, or the rule is refusing the
// packages it was written to protect.
func TestLintAcceptsTheShippedTraitUsage(t *testing.T) {
	dir := writeTraitPkg(t, "requires:\n  - { trait: identity, mode: oidc }\n  - { trait: ingress, mode: public }\n  - { trait: storage, class: bulk }\n")
	if got := issuesOfRule(t, dir, "trait"); len(got) != 0 {
		t.Errorf("shipped-shape trait usage produced issues: %v", got)
	}
}

// TestLintUsesADeclaredExample is the reason ParamDecl.Example exists.
// The auto-filler invents "lint-<name>", which is a fine stand-in for a
// domain and nonsense for a listen address — and the resulting failure
// blames the package for a value the author never chose.
func TestLintUsesADeclaredExample(t *testing.T) {
	dir := t.TempDir()
	body := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: app, version: 1.0.0, capability: app }
params:
  - { name: node, required: true }
  - { name: listen_addr, required: true, example: "127.0.0.1" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: app, node: "{{ .Params.node }}" }
    spec:
      image: "ghcr.io/example/app@sha256:` + strings.Repeat("a", 64) + `"
      ports: ["{{ .Params.listen_addr }}:8080:80"]
`
	if err := os.WriteFile(filepath.Join(dir, "package.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	issues, err := Lint(dir, LintOptions{})
	if err != nil {
		t.Fatalf("lint could not run: %v", err)
	}
	for _, i := range issues {
		if i.Rule == "expand" {
			t.Errorf("the declared example was not used: %s", i.Msg)
		}
	}
}

// A filler still wins over an example: the caller knows this run, and
// `wild pkg lint -set` must keep working for the one-off case.
func TestAFillerBeatsADeclaredExample(t *testing.T) {
	dir := t.TempDir()
	// The example is deliberately a value the resource validator refuses,
	// so "the filler won" and "the example won" are distinguishable from
	// Lint's return value alone.
	body := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: app, version: 1.0.0, capability: app }
params:
  - { name: node, required: true }
  - { name: vol, required: true, example: "Not A Valid Name!" }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: "{{ .Params.vol }}", node: "{{ .Params.node }}" }
    spec: { name: "{{ .Params.vol }}" }
`
	if err := os.WriteFile(filepath.Join(dir, "package.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if issues, err := Lint(dir, LintOptions{Fillers: map[string]string{"vol": "data"}}); err != nil {
		t.Fatalf("lint could not run: %v", err)
	} else if len(issues) != 0 {
		t.Fatalf("the filler did not win over the declared example: %v", issues)
	}
	// And without the filler, the example is what lint renders — proving
	// the previous assertion was not passing for some other reason.
	issues, err := Lint(dir, LintOptions{})
	if err != nil {
		t.Fatalf("lint could not run: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("the example was not used at all; this test proves nothing about precedence")
	}
}
