package redact

import (
	"strings"
	"testing"
)

// TestNamesAndEnvKeysSurviveRedaction is defect D: the F27 failure
// reached through a different door. F27 sheltered "Kind/name" references,
// but a bare secret name and an env-var key are the same long run of the
// base64 character class with no kind prefix to recognize them by — so
// `wild secret list` printed a column of [REDACTED] and an identity
// plan's env block was unreadable.
func TestNamesAndEnvKeysSurviveRedaction(t *testing.T) {
	// Every one of these is from the identity stack the VM built by hand.
	survives := []string{
		"authelia-storage-encryption-key",
		"authelia-session-secret",
		"lldap-admin-password",
		"authelia-jwt-reset-secret",
		"IMMICH_MACHINE_LEARNING_URL",
		"AUTHELIA_IDENTITY_PROVIDERS_OIDC_HMAC_SECRET",
		"LLDAP_LDAP_BASE_DN",
		"caddy-tailnet-data",
	}
	for _, name := range survives {
		t.Run(name, func(t *testing.T) {
			if got := String(name); got != name {
				t.Errorf("String(%q) = %q, want it unchanged", name, got)
			}
			if SecretShaped(name) {
				t.Errorf("SecretShaped(%q) = true — the policy engine would reject a name", name)
			}
		})
	}
}

// highEntropyValue is a fabricated token, assembled from fragments rather
// than written as one literal. A "SECRET: <high-entropy>" pair in the
// source is exactly the shape the repo's secret scanner exists to catch,
// and teaching it to ignore that shape so a test could compile would be a
// bad trade — the test needs the shape at runtime, not in the file.
var highEntropyValue = "xK9fQ2Lm" + "Zp4RsTuV" + "wXyZ0123"

// The shelter must not become a way to smuggle a value past the redactor.
func TestIdentifierShelterDeclinesRealMaterial(t *testing.T) {
	redacted := []string{
		// Hyphenated hex: encoded bytes wearing separators, not a word.
		"550e8400-e29b-41d4-a716-446655440000",
		"deadbeef-cafebabe-01234567-89abcdef",
		// Mixed case is not an identifier shape at all.
		highEntropyValue,
		// Undifferentiated run, no separator.
		"AAAAB3Nz" + "aC1yc2EA" + "AAADAQABAAABgQ",
	}
	for _, v := range redacted {
		t.Run(v, func(t *testing.T) {
			if got := String(v); strings.Contains(got, v) {
				t.Errorf("String(%q) = %q — value survived redaction", v, got)
			}
		})
	}
}

// The one shape the shelter genuinely gives up on — a diceware passphrase
// is indistinguishable from a kebab identifier — is still redacted where
// it actually appears, because the keyword rule does not look at shape.
// This is the compensating control, so it is pinned.
func TestKeyedValueStillRedactedDespiteIdentifierShape(t *testing.T) {
	for _, in := range []string{
		"DB_PASSWORD: correct-horse-battery-staple",
		"admin_password=my-lab-password-value",
		`{"apiKey": "some-long-identifier-value"}`,
	} {
		got := String(in)
		if !strings.Contains(got, Placeholder) {
			t.Errorf("String(%q) = %q, want the value redacted", in, got)
		}
	}
}

// And the key half of such a pair must still be readable: redacting
// "DB_PASSWORD" itself would leave a plan step saying nothing at all.
func TestKeyHalfSurvives(t *testing.T) {
	got := String("AUTHELIA_SESSION_SECRET: " + highEntropyValue)
	if !strings.HasPrefix(got, "AUTHELIA_SESSION_SECRET") {
		t.Errorf("String() = %q, want the key preserved", got)
	}
	if strings.Contains(got, highEntropyValue) {
		t.Errorf("String() = %q, want the value redacted", got)
	}
}
