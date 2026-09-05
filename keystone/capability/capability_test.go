package capability

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"

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

func TestExpandRejectsYAMLInjectingParam(t *testing.T) {
	pkg := simplePackage()
	// A param value with an embedded newline + YAML key would, without
	// validation, inject metadata.owner (or a mount/env/port) into the
	// rendered document. The value is interpolated into the node scalar.
	inject := "node-a\"}\nmetadata: { name: pwned, owner: system }\n#"
	_, err := (&Engine{}).Expand(pkg, map[string]string{"node": inject}, NewCatalog())
	if err == nil {
		t.Fatal("expected a param-injection rejection, got nil")
	}
	if te, ok := err.(*TemplateError); !ok {
		t.Fatalf("got %T, want *TemplateError: %v", err, err)
	} else if te.Package != "widget" {
		t.Fatalf("TemplateError package = %q, want widget", te.Package)
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
metadata: { name: immich, node: node-a }
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
	// Two: the OidcClient the template rendered, and the Secret its
	// SecretRef names, synthesised from the same fact.
	if len(res) != 2 {
		t.Fatalf("got %d resources, want 2 (the OidcClient and its secret): %+v", len(res), res)
	}
	r := res[0]
	if r.OidcClient == nil {
		t.Fatal("OidcClient spec missing")
	}
	sec := res[1]
	if sec.Kind != resource.KindSecret || sec.Secret == nil || sec.Secret.Name != "photos-oidc" {
		t.Fatalf("expected a Secret named photos-oidc alongside the client, got %+v", sec)
	}
	if sec.Metadata.Owner != "capability/photos" {
		t.Errorf("the synthesised Secret is owned by %q, want capability/photos", sec.Metadata.Owner)
	}
	if sec.Secret.Kind != "" {
		t.Errorf("a client secret should be random (empty kind), got %q", sec.Secret.Kind)
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
metadata: { name: checker, node: node-a }
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

// TestDeclaredSecretBeatsSynthesis pins which side owns a client secret.
//
// The engine synthesises a Secret from an OidcClient's SecretRef so that
// no consumer has to repeat the naming convention. It appended one
// unconditionally, so a package that declared the same Secret ITSELF got
// two resources with one name and the synthesised one last — silently
// replacing the package's own spec.
//
// documents-paperless is the case that matters: it declares its client
// secret `provided: true`, because Paperless needs the value in
// plaintext inside a JSON env document and Wildstone's secrets are
// write-only. Overriding that with a generated one puts random bytes
// where a shared secret was required, and it surfaces as OIDC login not
// working rather than as anything naming the secret.
func TestDeclaredSecretBeatsSynthesis(t *testing.T) {
	oidcTmpl := `apiVersion: wildstone.dev/v1
kind: OidcClient
metadata: { name: app, node: "{{ .Params.node }}" }
spec:
  clientId: app
  clientName: App
  redirectUris: ["https://app.example.com/cb"]
  scopes: [openid]
  authPolicy: two_factor
  secretRef: secret/app-oidc
`
	declaredTmpl := `apiVersion: wildstone.dev/v1
kind: Secret
metadata: { name: app-oidc, node: "{{ .Params.node }}" }
spec: { name: app-oidc, provided: true }
`
	for _, tc := range []struct {
		name         string
		templates    []string
		wantProvided bool
	}{
		{"package declares it", []string{oidcTmpl, declaredTmpl}, true},
		{"package says nothing", []string{oidcTmpl}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg := simplePackage()
			pkg.Templates = tc.templates
			out, err := NewEngine().Expand(pkg, map[string]string{"node": "n1"}, NewCatalog(pkg))
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			var secrets []*resource.SecretSpec
			for _, r := range out {
				if r.Kind == resource.KindSecret && r.Secret != nil && r.Secret.Name == "app-oidc" {
					secrets = append(secrets, r.Secret)
				}
			}
			if len(secrets) != 1 {
				t.Fatalf("want exactly one app-oidc Secret, got %d — a duplicate silently overrides the other", len(secrets))
			}
			if secrets[0].Provided != tc.wantProvided {
				t.Errorf("provided = %v, want %v", secrets[0].Provided, tc.wantProvided)
			}
		})
	}
}

// A package that publishes household events gets its publisher secret
// synthesised — provided, on the package's node — and can reference it
// as .Resolved.PublisherSecretRef (ADR-W022 A10).
func TestExpandSynthesisesPublisherSecret(t *testing.T) {
	pkg := simplePackage()
	pkg.Publishes = &pkgspec.PublishesSpec{Shape: "sidecar", Events: []pkgspec.PublishedEvent{{Type: "widget.pressed"}}}
	pkg.Templates = append(pkg.Templates, `apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: widget-shim, node: "{{ .Params.node }}" }
spec:
  image: "ghcr.io/example/shim@sha256:1111111111111111111111111111111111111111111111111111111111111111"
  env: { WILDSTONE_TOKEN: "{{ .Resolved.PublisherSecretRef }}" }
`)
	out, err := NewEngine().Expand(pkg, map[string]string{"node": "n1"}, NewCatalog(pkg))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	var secret *resource.Resource
	var shim *resource.Resource
	for i := range out {
		switch {
		case out[i].Kind == resource.KindSecret && out[i].Secret != nil && out[i].Secret.Name == "widgets-publisher":
			secret = &out[i]
		case out[i].Kind == resource.KindContainer && out[i].Metadata.Name == "widget-shim":
			shim = &out[i]
		}
	}
	if secret == nil {
		t.Fatal("no widgets-publisher Secret synthesised")
	}
	if !secret.Secret.Provided || secret.Metadata.Node != "n1" || secret.Metadata.Owner != "capability/widgets" {
		t.Fatalf("publisher secret = %+v / %+v", secret.Metadata, secret.Secret)
	}
	if shim == nil || shim.Container.Env["WILDSTONE_TOKEN"] != "secret/widgets-publisher" {
		t.Fatalf("shim env did not resolve PublisherSecretRef: %+v", shim)
	}
	if PublisherSecretName("widgets") != secret.Secret.Name {
		t.Fatalf("PublisherSecretName(widgets) = %q, want %q", PublisherSecretName("widgets"), secret.Secret.Name)
	}

	// A package that declares the Secret itself wins, as with OIDC.
	declared := simplePackage()
	declared.Publishes = pkg.Publishes
	declared.Templates = append(declared.Templates, `apiVersion: wildstone.dev/v1
kind: Secret
metadata: { name: widgets-publisher, node: "{{ .Params.node }}" }
spec: { name: widgets-publisher, provided: true }
`)
	out, err = NewEngine().Expand(declared, map[string]string{"node": "n1"}, NewCatalog(declared))
	if err != nil {
		t.Fatalf("expand declared: %v", err)
	}
	n := 0
	for _, r := range out {
		if r.Kind == resource.KindSecret && r.Secret.Name == "widgets-publisher" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one widgets-publisher Secret, got %d", n)
	}

	// No node param: the secret has nowhere to live, and Expand says so
	// rather than emitting a resource the reconciler cannot dispatch.
	nodeless := simplePackage()
	nodeless.Params = nil
	nodeless.Templates = []string{`apiVersion: wildstone.dev/v1
kind: Container
metadata: { name: widget-server, node: "n1" }
spec: { image: "ghcr.io/example/widget@sha256:1111111111111111111111111111111111111111111111111111111111111111" }
`}
	nodeless.Publishes = pkg.Publishes
	if _, err := NewEngine().Expand(nodeless, nil, NewCatalog(nodeless)); err == nil || !strings.Contains(err.Error(), "no node param") {
		t.Fatalf("nodeless publisher: err = %v, want a no-node-param error", err)
	}

	// A package that does not publish gets no publisher secret.
	out, err = NewEngine().Expand(simplePackage(), map[string]string{"node": "n1"}, NewCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if r.Kind == resource.KindSecret {
			t.Fatalf("non-publishing package grew a Secret: %+v", r)
		}
	}
}
