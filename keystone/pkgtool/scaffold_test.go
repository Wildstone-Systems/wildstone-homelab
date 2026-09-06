package pkgtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

// writeScaffold renders a shape into a temp directory and returns the
// package directory, as `wild pkg new` would leave it.
func writeScaffold(t *testing.T, shape string) string {
	t.Helper()
	root := t.TempDir()
	path, err := Scaffold(root, ScaffoldOptions{Name: "example-app", Shape: shape})
	if err != nil {
		t.Fatalf("Scaffold(%s): %v", shape, err)
	}
	return filepath.Dir(path)
}

// TestScaffoldLintsGreen is the whole reason this scaffold exists.
//
// A scaffold whose first lint fails teaches the reader that failing
// output is normal — which is precisely backwards for a project whose
// contract test is lint. So every shape must come out of the box with a
// digest pin, a Route the policy engine accepts, secret REFERENCES rather
// than values, no `container/` prefix in env, and a classification in the
// closed vocabulary.
//
// The identity package is on the catalog because the service shape
// requires identity:oidc, which is a real dependency on another package
// and not something a scaffold can satisfy alone — it is exactly the
// `-catalog` argument `wild pkg new` prints for the author.
func TestScaffoldLintsGreen(t *testing.T) {
	identity := filepath.Join(repoRoot(t), "packages", "identity")
	for _, shape := range ShapeNames() {
		t.Run(shape, func(t *testing.T) {
			dir := writeScaffold(t, shape)
			issues, err := Lint(dir, LintOptions{CatalogDirs: []string{identity}})
			if err != nil {
				t.Fatalf("lint could not run: %v", err)
			}
			for _, issue := range issues {
				t.Errorf("a freshly scaffolded package must lint clean: %s", issue)
			}
		})
	}
}

// TestEveryShapeLeavesTODOs. The scaffold's job is to be correct AND
// unfinished: a file that lints green and says nothing about what is
// still the author's decision is one they will ship as-is, image
// placeholder and all.
func TestEveryShapeLeavesTODOs(t *testing.T) {
	for _, shape := range ShapeNames() {
		content, err := Render(ScaffoldOptions{Name: "example-app", Shape: shape})
		if err != nil {
			t.Fatalf("Render(%s): %v", shape, err)
		}
		if n := strings.Count(string(content), "TODO"); n < 3 {
			t.Errorf("shape %s leaves only %d TODOs; the decisions a scaffold cannot make must be marked", shape, n)
		}
	}
}

// TestScaffoldNamesReachEverythingDerivedFromThem. The package name
// becomes a directory, resource names, a container name and — with
// dashes mapped to underscores — an event type namespace. A name that
// works in one and not another produces a validation error against a
// line the author never wrote.
func TestScaffoldNamesReachEverythingDerivedFromThem(t *testing.T) {
	for _, shape := range ShapeNames() {
		content, err := Render(ScaffoldOptions{Name: "example-app", Shape: shape})
		if err != nil {
			t.Fatalf("Render(%s): %v", shape, err)
		}
		body := string(content)
		if strings.Contains(body, "__") {
			t.Errorf("shape %s left an unsubstituted placeholder:\n%s", shape, body)
		}
		if strings.Contains(body, "example-app.something_happened") {
			t.Errorf("shape %s used a dashed capability as an event namespace; "+
				"an event type is [a-z0-9_] and would fail validation", shape)
		}
	}
	// The mapping itself, stated: a dashed capability is fine, and its
	// event namespace is the underscored form.
	content, err := Render(ScaffoldOptions{Name: "example-app", Shape: "config"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "example_app.something_happened") {
		t.Error("the config shape should namespace its events with the underscored capability")
	}
}

// TestScaffoldCapabilityAndProfileDefaultToTheName, because a first
// package has one of each and being made to type all three would teach
// that they are the same thing — which is exactly the belief that breaks
// when a second profile of the capability appears.
func TestScaffoldCapabilityAndProfileDefaultToTheName(t *testing.T) {
	dir := writeScaffold(t, "service")
	pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
	if err != nil {
		t.Fatalf("the scaffold must parse: %v", err)
	}
	if pkg.Metadata.Name != "example-app" || pkg.Metadata.Capability != "example-app" || pkg.Metadata.Profile != "example-app" {
		t.Errorf("got name/capability/profile %q/%q/%q",
			pkg.Metadata.Name, pkg.Metadata.Capability, pkg.Metadata.Profile)
	}

	// And they are separable, which is the case that matters: photos-immich
	// is capability "photos", profile "immich".
	content, err := Render(ScaffoldOptions{Name: "photos-immich", Capability: "photos", Profile: "immich", Shape: "service"})
	if err != nil {
		t.Fatal(err)
	}
	pkg2, err := pkgspec.Parse(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pkg2.Metadata.Capability != "photos" || pkg2.Metadata.Profile != "immich" {
		t.Errorf("got capability %q profile %q", pkg2.Metadata.Capability, pkg2.Metadata.Profile)
	}
}

// TestScaffoldRefusesToOverwrite. A scaffold is for a package that does
// not exist yet; the alternative is a command that can silently discard
// an afternoon's work because someone re-ran it with the wrong name.
func TestScaffoldRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := Scaffold(root, ScaffoldOptions{Name: "example-app", Shape: "config"}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	path := filepath.Join(root, "example-app", "package.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(root, ScaffoldOptions{Name: "example-app", Shape: "service"}); err == nil {
		t.Fatal("scaffolding over an existing package must fail")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the refused scaffold still modified the file")
	}
}

func TestScaffoldRejectsBadInput(t *testing.T) {
	for name, opts := range map[string]ScaffoldOptions{
		"unknown shape":     {Name: "example-app", Shape: "microservice"},
		"empty name":        {Name: "", Shape: "service"},
		"uppercase name":    {Name: "ExampleApp", Shape: "service"},
		"underscored name":  {Name: "example_app", Shape: "service"},
		"trailing dash":     {Name: "example-", Shape: "service"},
		"path traversal":    {Name: "../escape", Shape: "service"},
		"bad capability":    {Name: "example-app", Capability: "Example App", Shape: "service"},
		"no shape at all":   {Name: "example-app"},
		"leading digit":     {Name: "9lives", Shape: "service"},
		"dots in the name":  {Name: "example.app", Shape: "service"},
		"slash in the name": {Name: "example/app", Shape: "service"},
	} {
		if _, err := Render(opts); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

// TestCatalogSiblingsFindsTheRestOfTheCatalog, which is what makes the
// printed lint command correct rather than aspirational.
func TestCatalogSiblingsFindsTheRestOfTheCatalog(t *testing.T) {
	root := t.TempDir()
	if _, err := Scaffold(root, ScaffoldOptions{Name: "example-app", Shape: "config"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(root, ScaffoldOptions{Name: "other-app", Shape: "config"}); err != nil {
		t.Fatal(err)
	}
	// A directory with no package.yaml is not a package: a catalog is
	// allowed to hold a README's folder without it becoming a dependency.
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := CatalogSiblings(filepath.Join(root, "example-app"))
	if len(got) != 1 || filepath.Base(got[0]) != "other-app" {
		t.Errorf("siblings = %v, want just other-app", got)
	}
}

// TestShapeImageFlagMatchesTheTemplate. `wild pkg new` offers
// refresh-digests only to the shapes that pin an image, and a flag that
// drifted from the template would either withhold the command a scaffold
// needs or offer one that does nothing — and advice that does not apply
// is how a reader learns to skim the advice that does.
func TestShapeImageFlagMatchesTheTemplate(t *testing.T) {
	for _, s := range Shapes {
		content, err := Render(ScaffoldOptions{Name: "example-app", Shape: s.Name})
		if err != nil {
			t.Fatalf("Render(%s): %v", s.Name, err)
		}
		rendersImage := strings.Contains(string(content), "@"+placeholderDigest)
		if rendersImage != s.PinsImages {
			t.Errorf("shape %s: PinsImages = %v but the template %s render a digest-pinned image",
				s.Name, s.PinsImages, map[bool]string{true: "does", false: "does not"}[rendersImage])
		}
	}
}

// TestShapesAreDescribed. `wild pkg new` with no arguments is where an
// author chooses, and a one-word menu makes them choose by guessing —
// which is how a service that could have used the config shape ends up
// with an adapter container nobody needed.
func TestShapesAreDescribed(t *testing.T) {
	for _, s := range Shapes {
		if strings.TrimSpace(s.Summary) == "" {
			t.Errorf("shape %q has no summary", s.Name)
		}
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("shape %q has no description", s.Name)
		}
		if _, ok := LookupShape(s.Name); !ok {
			t.Errorf("shape %q is listed but not findable", s.Name)
		}
	}
	if _, ok := LookupShape("nonesuch"); ok {
		t.Error("LookupShape found a shape that does not exist")
	}
}
