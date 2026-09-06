package capability

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/trait"
)

// The trait-export mechanism (ADR-W025 A2) replaced a hardcoded lookup of
// "identity"/"oidc" inside buildResolved. These tests cover the two
// things that refactor could break silently: the engine's half of the
// contract with keystone/trait, and the shipped identity package's
// exports actually producing the values every consumer already depends
// on.

// repoRoot finds the checkout from this file's own path, so the tests
// find packages/ regardless of the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// TestExportFuncsMatchTheVocabulary pins the two halves of the export
// contract together.
//
// keystone/trait declares the function NAMES, and pkgspec parses an
// export template against those names alone — so a name declared there
// and not bound here would pass lint and then fail at expansion, during
// an install the operator has already approved. A name bound here and not
// declared there is the mirror image: a function that works and that lint
// calls a typo.
func TestExportFuncsMatchTheVocabulary(t *testing.T) {
	bound := exportFuncs(ingressorigin.Posture{})
	for _, name := range trait.ExportFuncs {
		if _, ok := bound[name]; !ok {
			t.Errorf("trait.ExportFuncs declares %q but exportFuncs does not bind it: "+
				"an export using it lints clean and fails at install", name)
		}
	}
	for name := range bound {
		found := false
		for _, declared := range trait.ExportFuncs {
			if declared == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("exportFuncs binds %q but trait.ExportFuncs does not declare it: "+
				"lint would refuse an export that in fact works", name)
		}
	}
}

// TestShippedIdentityPackageStillExportsThePortal loads the real
// packages/identity and asserts the three portal values, because the
// fixtures in resolvedorigin_test.go prove the MECHANISM and not that the
// catalog still uses it. Delete identity's exports block and every test
// in that file still passes; this one does not.
func TestShippedIdentityPackageStillExportsThePortal(t *testing.T) {
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "identity", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/identity/package.yaml: %v", err)
	}
	got := resolve(t, consumer(), map[string]string{"public_domain": "example.com"}, NewCatalog(pkg))
	for key, want := range map[string]string{
		"IssuerURL":     "https://auth.example.com",
		"SessionDomain": "example.com",
		"DashboardURL":  "https://home.example.com",
	} {
		if got[key] != want {
			t.Errorf("packages/identity no longer exports %s\n got %q\nwant %q", key, got[key], want)
		}
	}
}

// TestShippedIdentityExportsFollowTheTailnetPosture is the same package
// under the posture where the answer is not the declared host. The
// hardcoded version got this right; a data-driven one that reached for
// string concatenation instead of `origin` would not, and the symptom
// would be a login loop rather than a failure.
func TestShippedIdentityExportsFollowTheTailnetPosture(t *testing.T) {
	pkg, err := pkgspec.Load(filepath.Join(repoRoot(t), "packages", "identity", "package.yaml"))
	if err != nil {
		t.Fatalf("loading packages/identity/package.yaml: %v", err)
	}
	got := resolve(t, consumer(), map[string]string{"public_domain": "example.com"},
		NewCatalog(pkg).WithCurrent(tailnetState()))
	for key, want := range map[string]string{
		"IssuerURL":     "https://wildstone.tailnet.ts.net:7443",
		"SessionDomain": "wildstone.tailnet.ts.net",
		"DashboardURL":  "https://wildstone.tailnet.ts.net:443",
	} {
		if got[key] != want {
			t.Errorf("%s\n got %q\nwant %q", key, got[key], want)
		}
	}
}

// TestAnExportRenderingEmptyIsNotPublished. A portal origin built from a
// domain nobody has supplied is "https://auth.", which is not a worse URL
// than no URL — it is a service that starts, accepts a login attempt, and
// loops. Publishing nothing makes the consumer's own template fail on a
// missing key, which names the problem.
func TestAnExportRenderingEmptyIsNotPublished(t *testing.T) {
	provider := &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "identity", Capability: "identity"},
		Provides: []pkgspec.Provide{{Trait: "identity", Mode: "oidc", Exports: map[string]string{
			"IssuerURL": `{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}`,
		}}},
		// Required and undefaulted: nothing has supplied it yet.
		Params: []pkgspec.ParamDecl{{Name: "public_domain", Required: true}},
	}
	got := resolve(t, consumer(), map[string]string{}, NewCatalog(provider))
	if v, ok := got["IssuerURL"]; ok {
		t.Errorf("IssuerURL published as %q with no domain supplied; a consumer would render a URL that cannot work", v)
	}
}

// TestIntrinsicKeysCannotBeOverriddenByAnExport. OidcSecretRef and
// PublisherSecretRef are conventions the ENGINE also acts on — it
// synthesises the matching Secret resources — so a package that could
// redefine the reference would produce a template pointing at one name
// and a Secret created under another, with nothing comparing the two.
func TestIntrinsicKeysCannotBeOverriddenByAnExport(t *testing.T) {
	hostile := &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "hostile", Capability: "hostile"},
		Provides: []pkgspec.Provide{{Trait: "identity", Mode: "oidc", Exports: map[string]string{
			"OidcSecretRef": "secret/somewhere-else",
			"TailnetHost":   "attacker.example.com",
		}}},
	}
	got := resolve(t, consumer(), map[string]string{}, NewCatalog(hostile))
	if got["OidcSecretRef"] != "secret/photos-oidc" {
		t.Errorf("an export overrode the engine's own secret reference: %q", got["OidcSecretRef"])
	}
	if got["TailnetHost"] != "" {
		t.Errorf("an export overrode TailnetHost: %q", got["TailnetHost"])
	}
}

// TestTheFirstProviderOfAKeyWins states the tie-break, which is the rule
// the hardcoded lookup had (it took the first catalog package providing
// identity:oidc) and which map iteration would otherwise decide anew on
// every run.
func TestTheFirstProviderOfAKeyWins(t *testing.T) {
	mk := func(name, value string) *pkgspec.Package {
		return &pkgspec.Package{
			Metadata: pkgspec.Metadata{Name: name, Capability: name},
			Provides: []pkgspec.Provide{{Trait: "identity", Mode: "oidc", Exports: map[string]string{
				"IssuerURL": value,
			}}},
		}
	}
	for i := 0; i < 10; i++ {
		got := resolve(t, consumer(), map[string]string{}, NewCatalog(mk("first", "https://first"), mk("second", "https://second")))
		if got["IssuerURL"] != "https://first" {
			t.Fatalf("run %d: first provider did not win: %q", i, got["IssuerURL"])
		}
	}
}

// TestExportsRenderOverTheProvidersOwnParams. An export answers "what do
// I hand my consumers", so it can only offer what its own package was
// configured with — a consumer's unrelated param must not leak into a
// value attributed to the provider.
func TestExportsRenderOverTheProvidersOwnParams(t *testing.T) {
	provider := &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "identity", Capability: "identity"},
		Provides: []pkgspec.Provide{{Trait: "identity", Mode: "oidc", Exports: map[string]string{
			"IssuerURL": `{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}`,
		}}},
		Params: []pkgspec.ParamDecl{{Name: "public_domain", Default: "provider.example"}},
	}
	got := resolve(t, consumer(), map[string]string{"public_domain": "consumer.example"}, NewCatalog(provider))
	if got["IssuerURL"] != "https://auth.provider.example" {
		t.Errorf("provider's declared default should win over the requesting package's param, got %q", got["IssuerURL"])
	}
}

// TestStructuralTraitsComeFromTheVocabulary. ingress and storage were a
// map literal inside this package; if the engine and lint could disagree
// about which traits the system satisfies, one of them would be refusing
// a package the other installs.
func TestStructuralTraitsComeFromTheVocabulary(t *testing.T) {
	for _, name := range []string{"ingress", "storage"} {
		if !trait.IsStructural(name) {
			t.Errorf("%q must stay structural: the engine never looks for a provider of it", name)
		}
	}
	if trait.IsStructural("identity") {
		t.Error("identity is satisfied by a package, not by the system")
	}
	// An unknown trait must NOT be treated as structural — that would
	// turn every typo into a requirement that silently never fails.
	if trait.IsStructural("nonesuch") {
		t.Error("an unknown trait must not be structural")
	}
}

// TestAStructuralRequirementNeedsNoProvider is the property the map
// literal used to carry, asserted through the engine.
func TestAStructuralRequirementNeedsNoProvider(t *testing.T) {
	pkg := &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata:   pkgspec.Metadata{Name: "app", Version: "1.0.0", Capability: "app"},
		Requires: []pkgspec.Requirement{
			{Trait: "ingress", Mode: "public"},
			{Trait: "storage", Class: "bulk"},
		},
		Params: []pkgspec.ParamDecl{{Name: "node", Required: true}},
		Templates: []string{"apiVersion: wildstone.dev/v1\nkind: Volume\n" +
			"metadata: { name: data, node: \"{{ .Params.node }}\" }\nspec: { name: data }\n"},
	}
	if _, err := NewEngine().Expand(pkg, map[string]string{"node": "n1"}, NewCatalog()); err != nil {
		t.Fatalf("structural requirements must be satisfiable with an empty catalog: %v", err)
	}
}

// TestAnUnknownRequirementStillFails, so that making the vocabulary
// explicit did not accidentally make unknown traits free.
func TestAnUnknownRequirementStillFails(t *testing.T) {
	pkg := &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata:   pkgspec.Metadata{Name: "app", Version: "1.0.0", Capability: "app"},
		Requires:   []pkgspec.Requirement{{Trait: "nonesuch", Mode: "whatever"}},
		Params:     []pkgspec.ParamDecl{{Name: "node", Required: true}},
		Templates: []string{"apiVersion: wildstone.dev/v1\nkind: Volume\n" +
			"metadata: { name: data, node: \"{{ .Params.node }}\" }\nspec: { name: data }\n"},
	}
	_, err := NewEngine().Expand(pkg, map[string]string{"node": "n1"}, NewCatalog())
	if err == nil || !strings.Contains(err.Error(), "unsatisfied requirement") {
		t.Fatalf("want an unsatisfied-requirement error, got %v", err)
	}
}
