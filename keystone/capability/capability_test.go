package capability

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
)

func simplePackage() *pkgspec.Package {
	return &pkgspec.Package{
		ApiVersion: pkgspec.ApiVersion,
		Kind:       pkgspec.Kind,
		Metadata: pkgspec.Metadata{
			Name:       "widget",
			Version:    "1.0.0",
			Capability: "widgets",
		},
		Params: []pkgspec.ParamDecl{
			{Name: "node", Required: true},
		},
		Templates: []string{
			`apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: widget-server, node: "{{ .Params.node }}" }
spec: { image: "ghcr.io/example/widget@sha256:1111111111111111111111111111111111111111111111111111111111111111" }
`,
		},
	}
}

func TestExpandSubstitutesParamAndStampsOwner(t *testing.T) {
	pkg := simplePackage()
	res, err := (&Engine{}).Expand(pkg, map[string]string{"node": "node-a"}, NewCatalog())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1", len(res))
	}
	r := res[0]
	if r.Metadata.Owner != "capability/widgets" {
		t.Errorf("Owner = %q, want capability/widgets", r.Metadata.Owner)
	}
	if r.Metadata.Node != "node-a" {
		t.Errorf("Node = %q, want node-a", r.Metadata.Node)
	}
	if r.Container == nil {
		t.Fatal("Container spec missing")
	}
	if err := r.Validate(); err != nil {
		t.Errorf("resource.Validate: %v", err)
	}
}

func TestExpandMissingRequiredParam(t *testing.T) {
	pkg := simplePackage()
	_, err := (&Engine{}).Expand(pkg, map[string]string{}, NewCatalog())
	if err == nil {
		t.Fatal("expected MissingParamError")
	}
	mpe, ok := err.(*MissingParamError)
	if !ok {
		t.Fatalf("got %T, want *MissingParamError", err)
	}
	if mpe.Unknown {
		t.Errorf("Unknown = true, want false (missing-required case)")
	}
	if mpe.Param != "node" {
		t.Errorf("Param = %q, want node", mpe.Param)
	}
}

func TestExpandUnknownParam(t *testing.T) {
	pkg := simplePackage()
	_, err := (&Engine{}).Expand(pkg, map[string]string{"node": "n", "bogus": "x"}, NewCatalog())
	if err == nil {
		t.Fatal("expected error for unknown param")
	}
	mpe, ok := err.(*MissingParamError)
	if !ok {
		t.Fatalf("got %T, want *MissingParamError", err)
	}
	if !mpe.Unknown {
		t.Errorf("Unknown = false, want true")
	}
	if mpe.Param != "bogus" {
		t.Errorf("Param = %q, want bogus", mpe.Param)
	}
}

func TestExpandDefaultParamFilled(t *testing.T) {
	pkg := simplePackage()
	pkg.Params = append(pkg.Params, pkgspec.ParamDecl{Name: "region", Default: "home"})
	pkg.Templates = []string{
		`apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: widget-server, node: "{{ .Params.node }}" }
spec: { image: "ghcr.io/example/widget@sha256:1111111111111111111111111111111111111111111111111111111111111111", env: { REGION: "{{ .Params.region }}" } }
`,
	}
	res, err := (&Engine{}).Expand(pkg, map[string]string{"node": "node-a"}, NewCatalog())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if res[0].Container.Env["REGION"] != "home" {
		t.Errorf("REGION = %q, want default 'home'", res[0].Container.Env["REGION"])
	}
}

func consumerPackage() *pkgspec.Package {
	return &pkgspec.Package{
		Metadata: pkgspec.Metadata{
			Name:       "photos-immich",
			Version:    "1.0.0",
			Capability: "photos",
		},
		Requires: []pkgspec.Requirement{
			{Trait: "identity", Mode: "oidc"},
		},
		Params: []pkgspec.ParamDecl{
			{Name: "public_domain", Required: true},
		},
		Templates: []string{
			`apiVersion: wildstone.dev/v1
kind: OidcClient
metadata: { name: immich }
spec:
  clientId: immich
  clientName: Immich
  redirectUris: ["https://photos.{{ .Params.public_domain }}/auth/callback"]
  authPolicy: two_factor
  secretRef: "{{ .Resolved.OidcSecretRef }}"
`,
		},
	}
}

func identityProviderPackage() *pkgspec.Package {
	return &pkgspec.Package{
		Metadata: pkgspec.Metadata{
			Name:       "identity",
			Version:    "1.0.0",
			Capability: "identity",
		},
		Provides: []pkgspec.Provide{
			{Trait: "identity", Mode: "oidc"},
		},
		Params: []pkgspec.ParamDecl{
			{Name: "public_domain", Default: "example.internal"},
		},
	}
}

func TestExpandUnsatisfiedRequirementEmptyCatalog(t *testing.T) {
	pkg := consumerPackage()
	_, err := (&Engine{}).Expand(pkg, map[string]string{"public_domain": "example.internal"}, NewCatalog())
	if err == nil {
		t.Fatal("expected UnsatisfiedRequirementError")
	}
	ure, ok := err.(*UnsatisfiedRequirementError)
	if !ok {
		t.Fatalf("got %T, want *UnsatisfiedRequirementError", err)
	}
	if ure.Trait != "identity" || ure.Mode != "oidc" {
		t.Errorf("Trait/Mode = %s/%s, want identity/oidc", ure.Trait, ure.Mode)
	}
}

func TestExpandSatisfiedRequirementResolvesIssuerURL(t *testing.T) {
	pkg := consumerPackage()
	catalog := NewCatalog(identityProviderPackage())
	res, err := (&Engine{}).Expand(pkg, map[string]string{"public_domain": "example.internal"}, catalog)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1", len(res))
	}
	r := res[0]
	if r.OidcClient == nil {
		t.Fatal("OidcClient spec missing")
	}
	if r.OidcClient.SecretRef != "secret/photos-oidc" {
		t.Errorf("SecretRef = %q, want secret/photos-oidc", r.OidcClient.SecretRef)
	}
	if r.Metadata.Owner != "capability/photos" {
		t.Errorf("Owner = %q, want capability/photos", r.Metadata.Owner)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("resource.Validate: %v", err)
	}
}

// TestExpandIssuerURLTemplating exercises .Resolved.IssuerURL end-to-end by
// putting it directly into a template (redirectUris here, since OidcClient
// has no free-form field for an arbitrary URL — we assert on the rendered
// value via a Route template instead, which more naturally carries a host).
func TestExpandIssuerURLTemplating(t *testing.T) {
	pkg := &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "checker", Version: "1.0.0", Capability: "checker"},
		Requires: []pkgspec.Requirement{{Trait: "identity", Mode: "oidc"}},
		Templates: []string{
			`apiVersion: wildstone.dev/v1
kind: Route
metadata: { name: checker }
spec: { host: "{{ .Resolved.IssuerURL }}", upstream: "container/checker:80", auth: none, classification: family_public }
`,
		},
	}
	catalog := NewCatalog(identityProviderPackage())
	res, err := (&Engine{}).Expand(pkg, map[string]string{}, catalog)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := "https://auth.example.internal"
	if res[0].Route.Host != want {
		t.Errorf("Route.Host = %q, want %q", res[0].Route.Host, want)
	}
}

func TestExpandTemplateReferencesUndefinedField(t *testing.T) {
	pkg := &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "broken", Version: "1.0.0", Capability: "broken"},
		Templates: []string{
			`apiVersion: wildstone.dev/v1
kind: Volume
metadata: { name: v, owner: capability/broken }
spec: { name: "{{ .Params.doesNotExist }}" }
`,
		},
	}
	_, err := (&Engine{}).Expand(pkg, map[string]string{}, NewCatalog())
	if err == nil {
		t.Fatal("expected TemplateError for undefined field")
	}
	if _, ok := err.(*TemplateError); !ok {
		t.Fatalf("got %T, want *TemplateError", err)
	}
}

func TestExpandOwnerMismatchIsRejected(t *testing.T) {
	pkg := &pkgspec.Package{
		Metadata: pkgspec.Metadata{Name: "bad-owner", Version: "1.0.0", Capability: "bad"},
		Templates: []string{
			`apiVersion: wildstone.dev/v1
kind: Volume
metadata: { name: v, owner: capability/someone-else }
spec: { name: v }
`,
		},
	}
	_, err := (&Engine{}).Expand(pkg, map[string]string{}, NewCatalog())
	if err == nil {
		t.Fatal("expected TemplateError for owner mismatch")
	}
	if _, ok := err.(*TemplateError); !ok {
		t.Fatalf("got %T, want *TemplateError", err)
	}
}

func TestCatalogProvides(t *testing.T) {
	catalog := NewCatalog(identityProviderPackage())
	if !catalog.Provides("identity", "oidc") {
		t.Error("expected Provides(identity, oidc) = true")
	}
	if catalog.Provides("identity", "ldap") {
		t.Error("expected Provides(identity, ldap) = false")
	}
	if NewCatalog().Provides("identity", "oidc") {
		t.Error("expected empty catalog Provides = false")
	}
}
