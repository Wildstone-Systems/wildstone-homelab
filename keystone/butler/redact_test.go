package butler

import (
	"strings"
	"testing"
)

func TestRedact_HexSecretRedacted(t *testing.T) {
	in := "env.DB_PASSWORD_HASH: " + planted64HexSecret
	out := redact(in)
	if got := out; contains(got, planted64HexSecret) {
		t.Fatalf("redact() left the hex secret in place: %q", got)
	}
	if !contains(out, "[REDACTED]") {
		t.Fatalf("redact() did not mark the secret as redacted: %q", out)
	}
}

func TestRedact_SecretRefPreserved(t *testing.T) {
	in := "secretRef: secret/immich-db"
	out := redact(in)
	if !contains(out, "secret/immich-db") {
		t.Fatalf("redact() dropped the secret reference: %q", out)
	}
}

func TestRedact_MixedContent(t *testing.T) {
	in := "clientId: immich\nsecretRef: secret/immich-db\ntoken: " + planted64HexSecret + "\nplain: hello world"
	out := redact(in)
	if contains(out, planted64HexSecret) {
		t.Fatalf("hex secret survived redaction: %q", out)
	}
	if !contains(out, "secret/immich-db") {
		t.Fatalf("secret reference did not survive redaction: %q", out)
	}
	if !contains(out, "hello world") {
		t.Fatalf("redact() over-redacted ordinary content: %q", out)
	}
}

func TestRedact_KVPasswordLowEntropy(t *testing.T) {
	in := "password: hunter2"
	out := redact(in)
	if contains(out, "hunter2") {
		t.Fatalf("low-entropy password value survived redaction: %q", out)
	}
}

func TestRedact_EmptyString(t *testing.T) {
	if got := redact(""); got != "" {
		t.Fatalf("redact(\"\") = %q, want \"\"", got)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
