package resource

import "testing"

// TestProvidedSecretRejectsGenerationFields pins that provenance and
// shape stay orthogonal and consistent. Kind and LengthBytes describe how
// to GENERATE a secret; a provided one is never generated. Accepting
// both would let a package state a shape nothing enforces — an operator
// reading `kind: rsa_private_key, provided: true` would reasonably
// believe something checked that what they pasted was an RSA key.
func TestProvidedSecretRejectsGenerationFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec SecretSpec
		ok   bool
	}{
		{"provided alone", SecretSpec{Name: "cloudflared-tunnel-token", Provided: true}, true},
		{"generated with a kind", SecretSpec{Name: "authelia-oidc-jwks", Kind: "rsa_private_key"}, true},
		{"generated with a length", SecretSpec{Name: "hmac", LengthBytes: 48}, true},
		{"provided with a kind", SecretSpec{Name: "tok", Provided: true, Kind: "rsa_private_key"}, false},
		{"provided with a length", SecretSpec{Name: "tok", Provided: true, LengthBytes: 32}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Resource{
				ApiVersion: ApiVersion, Kind: KindSecret,
				Metadata: Metadata{Name: tc.spec.Name, Owner: "capability/test", Node: "node-a"},
				Secret:   &tc.spec,
			}
			err := r.Validate()
			if tc.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want rejected, got valid")
			}
		})
	}
}
