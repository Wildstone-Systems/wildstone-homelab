package resource

import "testing"

// The Ingress kind's ACME fields (ADR-W019 A9). The rule under test is
// that tls and acmeDns must AGREE — not merely that each is individually
// well-formed — because the failure mode of disagreement is silent. A
// profile that asks for a public certificate without naming a challenge
// provider does not fail to install; it installs, starts, and serves a
// certificate nobody trusts, which is indistinguishable from the
// internal-CA posture the operator chose this profile to escape.

func ingress(tls, acmeDNS string) *Resource {
	return &Resource{
		Kind:     KindIngress,
		Metadata: Metadata{Name: "ingress", Node: "node-a"},
		Ingress:  &IngressSpec{TLS: tls, ACMEDNS: acmeDNS},
	}
}

func TestPublicTLSRequiresAChallengeProvider(t *testing.T) {
	err := validateIngress(ingress("public", ""))
	if err == nil {
		t.Fatal("tls public with no acmeDns must be refused: the certificate could never be obtained")
	}
}

func TestChallengeProviderRefusedWithoutPublicTLS(t *testing.T) {
	for _, tls := range []string{"", "off", "internal"} {
		if err := validateIngress(ingress(tls, "cloudflare")); err == nil {
			t.Errorf("tls %q with acmeDns must be refused: nothing would use it, and the "+
				"profile would believe it had a public certificate", tls)
		}
	}
}

func TestUnknownChallengeProviderRefused(t *testing.T) {
	// A provider name is written verbatim into a Caddyfile global option
	// naming a module compiled into the binary. Wrong means the proxy
	// does not start — so it is caught here, not at 3am on a node.
	if err := validateIngress(ingress("public", "route53")); err == nil {
		t.Fatal("an unsupported provider must be refused at validation")
	}
}

func TestPublicWithCloudflareAccepted(t *testing.T) {
	if err := validateIngress(ingress("public", "cloudflare")); err != nil {
		t.Fatalf("the shipped combination must validate: %v", err)
	}
}

// The postures that shipped before A9 must be untouched by it.
func TestExistingPosturesStillValidate(t *testing.T) {
	for _, tls := range []string{"", "off", "internal"} {
		if err := validateIngress(ingress(tls, "")); err != nil {
			t.Errorf("posture %q regressed: %v", tls, err)
		}
	}
}
