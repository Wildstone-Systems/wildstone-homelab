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

// TestCommitHeadPreserved pins the second carve-out: a Statefile commit
// hash under a key that names one survives, because it is the field that
// tells a reviewer which desired state a plan was computed against — and
// git prints the same hashes in the clear one directory away. The shelter
// is keyed on the field name, so the same hash elsewhere still redacts.
func TestCommitHeadPreserved(t *testing.T) {
	const head = "9f2c1ab8e4d5f60718293a4b5c6d7e8f90a1b2c3"
	out := String(`{"basedOnHead":"` + head + `","apiToken":"` + hexSecret + `"}`)
	if !strings.Contains(out, `"basedOnHead":"`+head+`"`) {
		t.Fatalf("commit head redacted: %q", out)
	}
	if strings.Contains(out, hexSecret) {
		t.Fatalf("token survived beside the head: %q", out)
	}
	if out := String("value: " + head); strings.Contains(out, head) {
		t.Fatalf("hash under an unrelated key survived: %q", out)
	}
}

// TestEventIDPreserved pins the household-id shelter (ADR-W022 A2): a
// publisher-supplied id under eventId/correlationId survives whatever
// shape it has, while the same value under a secret-keyed field, or
// under no key at all, still redacts.
func TestEventIDPreserved(t *testing.T) {
	for _, id := range []string{
		"01J6ZK8Q2M7Y4N3R5T9V1W0X2A",                   // ULID
		"550e8400-e29b-41d4-a716-446655440000",         // UUID
		"ha-01J6ZK8Q2M7Y4N3R5T9V1W0X2A",                // prefixed ULID
		"pantry:meal:20260905T073000Z:mika",           // structured
		"7f3a9c2e1b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c", // content hash
		"school-pickup-suppression@41207:act",          // an engine decision id
	} {
		body := `{"seq":1,"eventId":"` + id + `","correlationId":"` + id + `","type":"person.left_home"}`
		if out := String(body); out != body {
			t.Errorf("id %q redacted: %s", id, out)
		}
		// The escaped form, as a body serialized inside a JSON string.
		escaped := `{\"eventId\":\"` + id + `\"}`
		if out := String(escaped); out != escaped {
			t.Errorf("escaped id %q redacted: %s", id, out)
		}
		// The same value under a secret key, or bare, is still a value.
		if out := String(`{"token":"` + id + `"}`); strings.Contains(out, id) {
			t.Errorf("id-shaped token survived: %s", out)
		}
	}
	// A secret beside a sheltered id: the id stays, the secret goes.
	in := `{"eventId":"01J6ZK8Q2M7Y4N3R5T9V1W0X2A","payload":{"apiKey":"KNOWNFAKE0123456789abcdefghij"}}`
	out := String(in)
	if !strings.Contains(out, "01J6ZK8Q2M7Y4N3R5T9V1W0X2A") || strings.Contains(out, "KNOWNFAKE") {
		t.Fatalf("adjacent: %s", out)
	}
	// Oversize under the key is not sheltered.
	long := strings.Repeat("A", 129)
	if out := String(`{"eventId":"` + long + `"}`); strings.Contains(out, long) {
		t.Fatalf("oversize id sheltered: %s", out)
	}
	if SecretShaped(`{"eventId":"01J6ZK8Q2M7Y4N3R5T9V1W0X2A"}`) {
		t.Fatal("SecretShaped fired on a keyed event id")
	}
}
