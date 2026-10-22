package pkgspec

import (
	"strings"
	"testing"
)

// TestParseRejectsUnknownTopLevelField proves that a typo'd top-level
// Package key (e.g. "metdata" for "metadata") is rejected rather than
// silently dropped.
func TestParseRejectsUnknownTopLevelField(t *testing.T) {
	data := `
apiVersion: wildstone.dev/v1
kind: Package
metdata:
  name: photos-immich
  version: 1.0.0
  capability: photos
  profile: immich
provides:
  - { trait: photos }
`
	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("Parse: expected error for unknown field \"metdata\", got nil")
	}
	if !strings.Contains(err.Error(), "field") {
		t.Fatalf("Parse: expected an unknown-field error, got: %v", err)
	}
}

// TestParseRejectsTypoMountKey is the B5 regression test: a typo'd Mount
// key ("readonly" instead of "readOnly") inside a lifecycle hook must be
// rejected at decode time instead of silently producing a writable mount
// that was intended to be read-only.
func TestParseRejectsTypoMountKey(t *testing.T) {
	data := `
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: photos-immich
  version: 1.0.0
  capability: photos
  profile: immich
lifecycle:
  preInstall:
    - image: "ghcr.io/example/hook@sha256:` + strings.Repeat("1", 64) + `"
      mounts:
        - { source: /data, target: /data, readonly: true }
`
	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("Parse: expected error for typo'd mount key \"readonly\", got nil")
	}
	if !strings.Contains(err.Error(), "field") {
		t.Fatalf("Parse: expected an unknown-field error, got: %v", err)
	}
}
