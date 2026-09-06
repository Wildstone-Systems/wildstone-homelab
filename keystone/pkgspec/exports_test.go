package pkgspec

import (
	"strings"
	"testing"
)

// Exports are what a provider hands its consumers (ADR-W025 A2). pkgspec
// checks them as STRUCTURE — the template parses, its functions exist,
// its params are declared — and leaves the vocabulary question (is this
// key one the trait carries) to lint, exactly as it does for
// policies.classifications.

func packageWithExports(exports map[string]string, params []ParamDecl) *Package {
	return &Package{
		ApiVersion: ApiVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "identity", Version: "1.0.0", Capability: "identity"},
		Provides:   []Provide{{Trait: "identity", Mode: "oidc", Exports: exports}},
		Params:     params,
	}
}

func TestValidExportsPass(t *testing.T) {
	pkg := packageWithExports(map[string]string{
		"IssuerURL":     `{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}`,
		"SessionDomain": `{{ with .Params.public_domain }}{{ sessionDomain . }}{{ end }}`,
	}, []ParamDecl{{Name: "public_domain", Required: true}})
	if err := pkg.Validate(); err != nil {
		t.Fatalf("the shipped export shape must validate: %v", err)
	}
}

// A misspelled function is the error worth catching at lint: caught here
// it is a package the author fixes, and missed it is an install that
// fails after an operator has already approved the plan.
func TestExportWithAnUnknownFunctionIsRejected(t *testing.T) {
	pkg := packageWithExports(map[string]string{
		"IssuerURL": `{{ orign "auth.example.com" }}`,
	}, nil)
	err := pkg.Validate()
	if err == nil {
		t.Fatal("want a validation error for an unknown template function")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("the message should list the functions that do exist: %v", err)
	}
}

// The check that pays for itself. An export naming a param its package
// does not declare renders empty, is dropped for being empty, and the
// consumer then fails on a missing .Resolved key — three packages away
// from the typo, with nothing connecting them.
func TestExportReferencingAnUndeclaredParamIsRejected(t *testing.T) {
	pkg := packageWithExports(map[string]string{
		"IssuerURL": `{{ origin (printf "auth.%s" .Params.public_domian) }}`,
	}, []ParamDecl{{Name: "public_domain", Required: true}})
	err := pkg.Validate()
	if err == nil {
		t.Fatal("want a validation error for an undeclared param reference")
	}
	if !strings.Contains(err.Error(), "public_domian") {
		t.Errorf("the message should name the reference: %v", err)
	}
}

// A param reference nested inside a function call must be found too. The
// first attempt reused the regexp that resolves defaults when a template
// is parsed as YAML, which matches only a bare "{{ .Params.x }}" — so
// every reference in the shipped identity package, all of which are
// nested, would have gone unchecked.
func TestNestedParamReferencesAreChecked(t *testing.T) {
	pkg := packageWithExports(map[string]string{
		"IssuerURL": `{{ with .Params.nope }}{{ origin . }}{{ end }}`,
	}, []ParamDecl{{Name: "public_domain", Required: true}})
	if err := pkg.Validate(); err == nil {
		t.Fatal("a param reference inside `with` must be checked like any other")
	}
}

func TestUnparseableExportIsRejected(t *testing.T) {
	pkg := packageWithExports(map[string]string{"IssuerURL": `{{ origin `}, nil)
	if err := pkg.Validate(); err == nil {
		t.Fatal("want a validation error for a template that does not parse")
	}
}

// An empty export is not a package saying "nothing here" — a package that
// has nothing to publish omits the key. An empty value is a half-written
// one, and publishing it would put an empty string into a consumer's URL.
func TestEmptyExportValueIsRejected(t *testing.T) {
	pkg := packageWithExports(map[string]string{"IssuerURL": "   "}, nil)
	if err := pkg.Validate(); err == nil {
		t.Fatal("want a validation error for an empty export value")
	}
}

// A package with no exports at all is the common case and must stay free
// of ceremony: most packages consume traits and provide none.
func TestNoExportsIsFine(t *testing.T) {
	pkg := &Package{
		ApiVersion: ApiVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "app", Version: "1.0.0", Capability: "app"},
		Provides:   []Provide{{Trait: "ingress", Mode: "private"}},
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("a package that exports nothing must validate: %v", err)
	}
}

// pkgspec must keep LOADING a package written against a vocabulary this
// binary does not have, so that the diagnostic comes from lint — which
// can list what does exist — rather than from a parser refusing to start.
func TestAnUnknownTraitStillParses(t *testing.T) {
	pkg, err := Parse([]byte(`apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: app, version: 1.0.0, capability: app }
requires:
  - { trait: something-new, mode: v2 }
provides:
  - { trait: something-new, mode: v2 }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("vocabulary is lint's question, not the parser's: %v", err)
	}
}

// Exports are round-tripped from YAML, not only constructed in Go: the
// shipped identity package writes them as a nested block, and a mapping
// this decoder silently dropped would leave every consumer without an
// issuer and nothing failing at load.
func TestExportsRoundTripFromYAML(t *testing.T) {
	pkg, err := Parse([]byte(`apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: identity, version: 1.0.0, capability: identity }
params:
  - { name: public_domain, required: true }
provides:
  - trait: identity
    mode: oidc
    exports:
      IssuerURL: '{{ with .Params.public_domain }}{{ origin (printf "auth.%s" .) }}{{ end }}'
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got := pkg.Provides[0].Exports["IssuerURL"]
	if !strings.Contains(got, "origin") {
		t.Fatalf("exports did not survive decoding: %q", got)
	}
}
