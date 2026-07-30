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
