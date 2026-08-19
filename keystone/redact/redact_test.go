package redact

import (
	"strings"
	"testing"
)

const hexSecret = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

func TestSecretValueRedacted(t *testing.T) {
	out := String("DB_PASSWORD: " + hexSecret)
	if strings.Contains(out, hexSecret) {
		t.Fatalf("hex secret survived: %q", out)
	}
	if !strings.Contains(out, Placeholder) {
		t.Fatalf("no placeholder in %q", out)
	}
}

func TestSecretRefPreserved(t *testing.T) {
	out := String("secretRef: secret/immich-db")
	if !strings.Contains(out, "secret/immich-db") {
		t.Fatalf("secret reference dropped: %q", out)
	}
}

// TestDigestPinPreserved pins the ADR-W007 A5 carve-out: sha256 content
// digests are integrity pins, not secrets — redacting them would break
// round-tripping a Container resource through the public API.
func TestDigestPinPreserved(t *testing.T) {
	image := "ghcr.io/immich-app/immich-server@sha256:" + hexSecret
	out := String("image: " + image)
	if !strings.Contains(out, image) {
		t.Fatalf("digest pin mangled: %q", out)
	}
	// The same hex WITHOUT the sha256: prefix still redacts.
	out = String("value: " + hexSecret)
	if strings.Contains(out, hexSecret) {
		t.Fatalf("bare hex survived: %q", out)
	}
}

// TestDigestNextToSecret ensures the shelter is per-occurrence: a digest
// pin and a real secret in the same document each get their own fate.
func TestDigestNextToSecret(t *testing.T) {
	in := "image: app@sha256:" + hexSecret + "\napi_key: " + strings.Repeat("a1b2", 10)
	out := String(in)
	if !strings.Contains(out, "sha256:"+hexSecret) {
		t.Fatalf("digest pin lost: %q", out)
	}
	if strings.Contains(out, strings.Repeat("a1b2", 10)) {
		t.Fatalf("api key survived: %q", out)
	}
}
