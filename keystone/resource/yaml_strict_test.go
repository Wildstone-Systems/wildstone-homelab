package resource

import (
	"strings"
	"testing"
)

// TestUnmarshalRejectsUnknownEnvelopeField proves that a typo'd top-level
// envelope key (e.g. "meta" for "metadata") is rejected rather than
// silently dropped.
func TestUnmarshalRejectsUnknownEnvelopeField(t *testing.T) {
	data := []byte(`
apiVersion: wildstone.dev/v1
kind: Volume
meta:
  name: jellyfin-config
  owner: capability/media
spec:
  name: jellyfin-config
`)
	_, err := Unmarshal(data)
	if err == nil {
		t.Fatal("Unmarshal: expected error for unknown envelope field \"meta\", got nil")
	}
	if !strings.Contains(err.Error(), "field") {
		t.Fatalf("Unmarshal: expected an unknown-field error, got: %v", err)
	}
}

// TestUnmarshalRejectsTypoMountKey is the B5 regression test: a typo'd
// Mount key ("readonly" instead of "readOnly") must be rejected at decode
// time instead of silently producing a writable mount that was intended to
// be read-only.
func TestUnmarshalRejectsTypoMountKey(t *testing.T) {
	data := []byte(`
apiVersion: wildstone.dev/v1
kind: Container
metadata:
  name: jellyfin
  owner: capability/media
  node: node-a
spec:
  image: ghcr.io/example/app@sha256:` + strings.Repeat("a", 64) + `
  mounts:
    - source: /data/jellyfin
      target: /config
      readonly: true
`)
	_, err := Unmarshal(data)
	if err == nil {
		t.Fatal("Unmarshal: expected error for typo'd mount key \"readonly\", got nil")
	}
	if !strings.Contains(err.Error(), "field") {
		t.Fatalf("Unmarshal: expected an unknown-field error, got: %v", err)
	}
}
