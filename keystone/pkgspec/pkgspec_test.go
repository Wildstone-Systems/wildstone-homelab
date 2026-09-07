package pkgspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const wellFormed = `
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: photos-immich
  version: 1.0.0
  capability: photos
  profile: immich
requires:
  - { trait: identity, mode: oidc }
params:
  - { name: public_domain, required: true }
  - { name: photos_subdomain, default: photos }
policies:
  classifications: [family_public, family_private]
  dataSensitivity: high
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: immich-server, owner: capability/photos }
    spec: { image: "ghcr.io/immich-app/immich-server@sha256:1111111111111111111111111111111111111111111111111111111111111111" }
lifecycle:
  preInstall:
    - image: "ghcr.io/example/hook@sha256:1111111111111111111111111111111111111111111111111111111111111111"
      mounts:
        - { source: /data, target: /data }
  postInstall: []
agentTools:
  - { name: search_photos, risk: domestic }
`

func TestLoadAndParseWellFormed(t *testing.T) {
	pkg, err := Parse([]byte(wellFormed))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if pkg.Metadata.Name != "photos-immich" {
		t.Errorf("Name = %q", pkg.Metadata.Name)
	}
	if pkg.Metadata.Capability != "photos" {
		t.Errorf("Capability = %q", pkg.Metadata.Capability)
	}
	if len(pkg.Params) != 2 {
		t.Fatalf("Params = %d, want 2", len(pkg.Params))
	}
	if len(pkg.Templates) != 1 {
		t.Fatalf("Templates = %d, want 1", len(pkg.Templates))
	}
	if len(pkg.Lifecycle.PreInstall) != 1 {
		t.Fatalf("PreInstall = %d, want 1", len(pkg.Lifecycle.PreInstall))
	}
	if len(pkg.AgentTools) != 1 {
		t.Fatalf("AgentTools = %d, want 1", len(pkg.AgentTools))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "package.yaml")
	if err := os.WriteFile(path, []byte(wellFormed), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Metadata.Name != pkg.Metadata.Name {
		t.Errorf("Load mismatch: %q vs %q", loaded.Metadata.Name, pkg.Metadata.Name)
	}
}

func TestValidateRejectsMissingName(t *testing.T) {
	pkg, err := Parse([]byte(strings.Replace(wellFormed, "name: photos-immich", "name: \"\"", 1)))
	if err != nil {
		t.Fatal(err)
	}
	err = pkg.Validate()
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("got %T, want *ValidationError", err)
	}
	if ve.Field != "metadata.name" {
		t.Errorf("Field = %q", ve.Field)
	}
}

func TestValidateRejectsMissingVersion(t *testing.T) {
	pkg, err := Parse([]byte(strings.Replace(wellFormed, "version: 1.0.0", "version: \"\"", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.Validate(); err == nil {
		t.Fatal("expected error for missing version")
	}
}

func TestValidateRejectsMissingCapability(t *testing.T) {
	pkg, err := Parse([]byte(strings.Replace(wellFormed, "capability: photos", "capability: \"\"", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.Validate(); err == nil {
		t.Fatal("expected error for missing capability")
	}
}

func TestValidateRejectsMalformedLifecycleHook(t *testing.T) {
	const bad = `
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: bad-hook
  version: 1.0.0
  capability: bad
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: v, owner: capability/bad }
    spec: { name: v }
lifecycle:
  preInstall:
    - mounts:
        - { source: /data, target: /data }
`
	pkg, err := Parse([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	err = pkg.Validate()
	if err == nil {
		t.Fatal("expected validation error for hook missing image")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("got %T, want *ValidationError", err)
	}
	if !strings.Contains(ve.Field, "preInstall") || !strings.Contains(ve.Field, "image") {
		t.Errorf("Field = %q, want to mention preInstall and image", ve.Field)
	}
}

func TestValidateRejectsMalformedAgentTool(t *testing.T) {
	const bad = `
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: bad-tool
  version: 1.0.0
  capability: bad
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: v, owner: capability/bad }
    spec: { name: v }
agentTools:
  - { name: search_photos, risk: extreme }
`
	pkg, err := Parse([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	err = pkg.Validate()
	if err == nil {
		t.Fatal("expected validation error for bad risk tier")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("got %T, want *ValidationError", err)
	}
	if !strings.Contains(ve.Field, "agentTools") || !strings.Contains(ve.Field, "risk") {
		t.Errorf("Field = %q, want to mention agentTools and risk", ve.Field)
	}
}

// TestValidateNeverExecutesLifecycleOrAgentTools documents (trivially, by
// construction — Validate contains no exec/os.StartProcess/http calls) that
// Validate is pure data validation. The hook below points at a non-existent
// image reference and an unresolvable host path; if Validate ever executed
// it, this test would hang, fail, or error on missing binaries/paths rather
// than returning a clean nil.
func TestValidateNeverExecutesLifecycleOrAgentTools(t *testing.T) {
	const pkgYAML = `
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: no-exec
  version: 1.0.0
  capability: no-exec
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: v, owner: capability/no-exec }
    spec: { name: v }
lifecycle:
  preInstall:
    - image: "this-image-does-not-exist-anywhere@sha256:1111111111111111111111111111111111111111111111111111111111111111"
      mounts:
        - { source: /this/path/does/not/exist, target: /data }
agentTools:
  - { name: definitely_not_a_real_tool, risk: high }
`
	pkg, err := Parse([]byte(pkgYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("Validate returned an error, meaning it inspected but did not execute: %v", err)
	}
}

func TestParseAndValidateRejectMalformedYAML(t *testing.T) {
	_, err := Parse([]byte("not: [valid"))
	if err == nil {
		t.Fatal("expected parse error for malformed YAML")
	}
}

func TestValidatePublishes(t *testing.T) {
	base := func() *Package {
		pkg, err := Parse([]byte(wellFormed))
		if err != nil {
			t.Fatal(err)
		}
		return pkg
	}
	good := &PublishesSpec{Shape: "sidecar", Events: []PublishedEvent{
		{Type: "person.left_home", Subject: "member", Fields: []EventField{{Name: "zone", Type: "string"}}},
		{Type: "calendar.school.closed"},
	}}
	pkg := base()
	pkg.Publishes = good
	if err := pkg.Validate(); err != nil {
		t.Fatalf("well-formed publishes: %v", err)
	}

	for name, tc := range map[string]struct {
		spec  *PublishesSpec
		field string
	}{
		"bad shape":      {&PublishesSpec{Shape: "logtail", Events: good.Events}, "publishes.shape"},
		"no events":      {&PublishesSpec{Shape: "native"}, "publishes.events"},
		"flat type":      {&PublishesSpec{Shape: "native", Events: []PublishedEvent{{Type: "left_home"}}}, "publishes.events[0].type"},
		"duplicate type": {&PublishesSpec{Shape: "native", Events: []PublishedEvent{{Type: "a.b"}, {Type: "a.b"}}}, "publishes.events[1].type"},
		"bad subject":    {&PublishesSpec{Shape: "native", Events: []PublishedEvent{{Type: "a.b", Subject: "pet"}}}, "publishes.events[0].subject"},
		"bad field type": {&PublishesSpec{Shape: "native", Events: []PublishedEvent{{Type: "a.b", Fields: []EventField{{Name: "x", Type: "blob"}}}}}, "publishes.events[0].fields[0].type"},
		"dup field":      {&PublishesSpec{Shape: "native", Events: []PublishedEvent{{Type: "a.b", Fields: []EventField{{Name: "x", Type: "string"}, {Name: "x", Type: "bool"}}}}}, "publishes.events[0].fields[1].name"},
	} {
		pkg := base()
		pkg.Publishes = tc.spec
		err := pkg.Validate()
		ve, ok := err.(*ValidationError)
		if !ok {
			t.Errorf("%s: err = %v, want *ValidationError", name, err)
			continue
		}
		if ve.Field != tc.field {
			t.Errorf("%s: field = %q, want %q", name, ve.Field, tc.field)
		}
	}

	// A YAML round trip: the block parses from a document the way a
	// package author writes it.
	doc := strings.Replace(wellFormed, "agentTools:", `publishes:
  shape: config
  events:
    - { type: person.arrived_home, subject: member, description: "HA person entity entered the home zone" }
agentTools:`, 1)
	parsed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("parsed publishes: %v", err)
	}
	if parsed.Publishes == nil || parsed.Publishes.Shape != "config" || parsed.Publishes.Events[0].Subject != "member" {
		t.Fatalf("publishes = %+v", parsed.Publishes)
	}
}

func TestValidateExternalConfig(t *testing.T) {
	base := func() *Package {
		pkg, err := Parse([]byte(wellFormed))
		if err != nil {
			t.Fatal(err)
		}
		return pkg
	}
	pkg := base()
	pkg.ExternalConfig = []ExternalConfigDecl{
		{Filename: "wildstone.yaml", Consumer: "Home Assistant", Reason: "runs outside Wildstone"},
	}
	if err := pkg.Validate(); err != nil {
		t.Fatalf("well-formed externalConfig: %v", err)
	}

	for name, tc := range map[string]struct {
		decls []ExternalConfigDecl
		field string
	}{
		"no filename":  {[]ExternalConfigDecl{{Consumer: "HA", Reason: "r"}}, "externalConfig[0].filename"},
		"no consumer":  {[]ExternalConfigDecl{{Filename: "f.yaml", Reason: "r"}}, "externalConfig[0].consumer"},
		"no reason":    {[]ExternalConfigDecl{{Filename: "f.yaml", Consumer: "HA"}}, "externalConfig[0].reason"},
		"blank reason": {[]ExternalConfigDecl{{Filename: "f.yaml", Consumer: "HA", Reason: "   "}}, "externalConfig[0].reason"},
		"duplicate": {[]ExternalConfigDecl{
			{Filename: "f.yaml", Consumer: "HA", Reason: "r"},
			{Filename: "f.yaml", Consumer: "HA", Reason: "r"},
		}, "externalConfig[1].filename"},
	} {
		pkg := base()
		pkg.ExternalConfig = tc.decls
		err := pkg.Validate()
		ve, ok := err.(*ValidationError)
		if !ok {
			t.Errorf("%s: err = %v, want *ValidationError", name, err)
			continue
		}
		if ve.Field != tc.field {
			t.Errorf("%s: field = %q, want %q", name, ve.Field, tc.field)
		}
	}

	// It parses from a document the way an author writes it.
	doc := strings.Replace(wellFormed, "agentTools:", `externalConfig:
  - { filename: wildstone.yaml, consumer: Home Assistant, reason: "runs outside Wildstone" }
agentTools:`, 1)
	parsed, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("parsed externalConfig: %v", err)
	}
	if len(parsed.ExternalConfig) != 1 || parsed.ExternalConfig[0].Consumer != "Home Assistant" {
		t.Fatalf("externalConfig = %+v", parsed.ExternalConfig)
	}
}

// The secrets an operator must supply are read from the package, not
// guessed from a convention: the two chat transports name theirs
// differently, and a form that assumed either would write the token into
// a secret nothing reads.
func TestProvidedSecretsComeFromThePackage(t *testing.T) {
	pkg := &Package{Templates: []string{
		"apiVersion: wildstone.dev/v1\nkind: Secret\nmetadata: { name: chat-matrix-family-token, node: n1 }\nspec: { name: chat-matrix-family-token, provided: true }\n",
		"apiVersion: wildstone.dev/v1\nkind: Secret\nmetadata: { name: chat-matrix-family-generated, node: n1 }\nspec: { name: chat-matrix-family-generated }\n",
		"apiVersion: wildstone.dev/v1\nkind: Container\nmetadata: { name: chat-matrix-family, node: n1 }\nspec: { image: x }\n",
	}}
	got := ProvidedSecrets(pkg)
	if len(got) != 1 || got[0] != "chat-matrix-family-token" {
		t.Fatalf("ProvidedSecrets = %v, want only the provided one", got)
	}
}

// Every chat package in the shipped catalog declares exactly the two
// provided secrets the Console's connect flow collects and mints: the
// vendor's token, and the adapter's own Keystone credential.
func TestShippedChatPackagesDeclareTheirSecrets(t *testing.T) {
	for _, name := range []string{
		"chat-telegram-family", "chat-telegram-admin", "chat-matrix-family", "chat-matrix-admin",
	} {
		pkg, err := Load(filepath.Join("..", "..", "packages", name, "package.yaml"))
		if err != nil {
			t.Fatalf("loading %s: %v", name, err)
		}
		got := ProvidedSecrets(pkg)
		if len(got) != 2 {
			t.Fatalf("%s: provided secrets = %v, want the vendor token and the adapter credential", name, got)
		}
		var vendor []string
		for _, s := range got {
			if s != name+"-adapter" {
				vendor = append(vendor, s)
			}
		}
		if len(vendor) != 1 {
			t.Fatalf("%s: could not tell the vendor token from the adapter credential in %v", name, got)
		}
	}
}
