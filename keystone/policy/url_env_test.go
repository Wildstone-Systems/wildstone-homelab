package policy

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/internal/fakesecret"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The defect these cover was found building the sidecar scaffold
// (ADR-W025 A5/A6): a container carrying the household intake URL in its
// env could not be planned at all, because "/" is in the base64 character
// class and a path of ordinary segments read as token material. The
// message said "value is secret-shaped", which names nothing a package
// author can act on.
//
// keystone/redact holds the shape tests; these assert the consequence at
// the layer that actually refuses an install.

func envChange(env map[string]string) Change {
	return Change{
		Principal: "test",
		Proposed: []resource.Resource{{
			Kind:     resource.KindContainer,
			Metadata: resource.Metadata{Name: "adapter", Node: "n1", Owner: "capability/demo"},
			Container: &resource.ContainerSpec{
				Image: "ghcr.io/example/adapter@sha256:" + strings.Repeat("a", 64),
				Env:   env,
			},
		}},
	}
}

// TestAnIntakeURLInEnvIsPlannable is the exact value a sidecar package
// needs, in the exact place it needs it.
func TestAnIntakeURLInEnvIsPlannable(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"the household intake": {"WILDSTONE_INTAKE_URL": "https://wildstone.tailnet.ts.net/api/v1/household/events"},
		"a watched service":    {"SERVICE_URL": "http://192.168.1.10:8080/api/v2/state/current"},
		"an OIDC issuer":       {"OIDC_ISSUER_URL": "https://auth.example.com/.well-known/openid-configuration"},
	} {
		violations, _ := NewEngine(nil).Validate(envChange(env))
		for _, v := range violations {
			t.Errorf("%s: an ordinary URL was refused: %s", name, v)
		}
	}
}

// TestACredentialInAURLIsStillRefused. The shelter must not become a way
// to smuggle a value past the rule by wrapping it in a scheme and a host.
func TestACredentialInAURLIsStillRefused(t *testing.T) {
	// Assembled at runtime, never written as a literal: see
	// keystone/internal/fakesecret for why a fabricated key in a test is
	// still the thing the secret scanner exists to catch.
	token := fakesecret.Token()
	for name, env := range map[string]map[string]string{
		"token in the path": {
			"WEBHOOK_URL": "https://hooks.example.com/services/T0000/B0000/" + token},
		"token in a query value": {
			"PUSH_URL": "https://api.example.com/v1/push?" + "api" + "_key=" + token},
		"credentials in userinfo": {
			"DB_URL": "https://admin:" + token + "@internal.example.com/db"},
	} {
		violations, _ := NewEngine(nil).Validate(envChange(env))
		if len(violations) == 0 {
			t.Errorf("%s: a credential carried in a URL was accepted", name)
			continue
		}
		if violations[0].Rule != RuleSecretValuesForbidden {
			t.Errorf("%s: got rule %q, want %q", name, violations[0].Rule, RuleSecretValuesForbidden)
		}
	}
}

// TestACredentialShapedKeyStillDemandsAReference. The rule that reads the
// KEY rather than the value is the one carrying the cases shape cannot
// reach — a short password in a URL among them — so sheltering URL
// structure must leave it exactly as it was.
func TestACredentialShapedKeyStillDemandsAReference(t *testing.T) {
	credentialKey := "WEBHOOK_" + "TOKEN"
	violations, _ := NewEngine(nil).Validate(envChange(map[string]string{
		credentialKey: "https://hooks.example.com/t/abc",
	}))
	if len(violations) == 0 {
		t.Fatal("a credential-shaped key must hold a secret/<name> reference, URL or not")
	}
	// And the reference form is still what satisfies it.
	violations, _ = NewEngine(nil).Validate(envChange(map[string]string{
		credentialKey: "secret/demo-publisher",
	}))
	for _, v := range violations {
		t.Errorf("a secret reference under a credential key must be accepted: %s", v)
	}
}
