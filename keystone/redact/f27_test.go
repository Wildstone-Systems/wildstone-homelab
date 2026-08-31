package redact

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// TestResourceRefsSurviveRedaction pins F27 with the exact strings the
// validation reported. Plan review by a human is the safety claim, so a
// plan step that reads "Create [REDACTED]" is not a cosmetic defect.
func TestResourceRefsSurviveRedaction(t *testing.T) {
	for _, want := range []string{
		"Volume/caddy-tailnet-data",       // the first-party ingress package's own plan
		"Container/caddy-tailnet",         //
		"Volume/immich-postgres-data",     // 27 chars, reported redacted
		"Volume/aaaaaaaaaaaaaaaaa",        // 24 chars, the reported threshold
		"Route/kuma.example.internal",     // dots in the name
		"BackupPlan/nightly-offsite-copy", //
	} {
		in := "Create " + want
		if got := String(in); !strings.Contains(got, want) {
			t.Errorf("String(%q) = %q, want the reference intact", in, got)
		}
	}

	// An error message is the harder case: no adjacent field carries the
	// name, so redacting it destroys the only copy.
	in := "apply Volume/immich-postgres-data failed: node offline"
	if got := String(in); got != in {
		t.Errorf("String(%q) = %q, want it unchanged", in, got)
	}
}

// TestEveryKindIsSheltered fails when a kind is added to the registry and
// the shelter is not rebuilt from it — the shelter reads resource.Kinds()
// precisely so that cannot happen, and this is what proves it.
func TestEveryKindIsSheltered(t *testing.T) {
	for _, k := range resource.Kinds() {
		ref := string(k) + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if got := String("Create " + ref); !strings.Contains(got, ref) {
			t.Errorf("kind %s is not sheltered: %q", k, got)
		}
	}
}

// TestShelterDoesNotHideRealSecrets is the other half. Over-redaction is
// the safe direction; a shelter that swallowed real tokens would not be.
func TestShelterDoesNotHideRealSecrets(t *testing.T) {
	for _, secret := range []string{
		"sk-aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
		"ghp_1234567890abcdefghijklmnopqrstuvwxyzAB",
		"5f4dcc3b5aa765d61d8327deb882cf995f4dcc3b5aa765d61d8327deb882cf99",
	} {
		if got := String("token: " + secret); strings.Contains(got, secret) {
			t.Errorf("secret survived redaction: %q", got)
		}
		// Nor may prefixing a kind smuggle one through a whole word.
		if got := String("Volume/x " + secret); strings.Contains(got, secret) {
			t.Errorf("secret survived beside a sheltered ref: %q", got)
		}
	}
}

// TestCommitShelterSurvivesJSONEscaping pins F32: String also runs over
// JSON that was itself serialized into a JSON string, where the field
// reads \"basedOnHead\":\"…\". The shelter missed that, the hash fell to
// the hex rule, and one document reported the same field two ways.
func TestCommitShelterSurvivesJSONEscaping(t *testing.T) {
	const hash = "015ef04480e32df1a8b5a8e7856f4efedb75146e"
	for _, in := range []string{
		`{"basedOnHead":"` + hash + `"}`,
		`{\"basedOnHead\":\"` + hash + `\"}`,
		`basedOnHead: ` + hash,
	} {
		if got := String(in); !strings.Contains(got, hash) {
			t.Errorf("String(%q) redacted the commit ref: %q", in, got)
		}
	}
	// A bare hash with no field naming it still redacts — the shelter is
	// keyed on the key, not the value's shape.
	bare := "leftover " + hash
	if got := String(bare); strings.Contains(got, hash) {
		t.Errorf("bare hash survived: %q", got)
	}
}
