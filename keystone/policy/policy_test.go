package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func route(name, classification, auth string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindRoute,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: name + ".example.internal", Upstream: "10.0.0.1:80",
			Auth: auth, Classification: classification,
		},
	}
}

func container(name string, env map[string]string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindContainer,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Container: &resource.ContainerSpec{
			Image: "reg.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Env:   env,
		},
	}
}

func ruleIDs(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Rule)
	}
	return out
}

func TestRules(t *testing.T) {
	e := NewEngine(nil)
	cases := []struct {
		name      string
		change    Change
		wantRules []string
	}{
		{
			name:   "clean public route with auth",
			change: Change{Proposed: []resource.Resource{route("app", "family_public", "authelia_oidc")}},
		},
		{
			name:      "unknown classification",
			change:    Change{Proposed: []resource.Resource{route("app", "totally_public", "authelia_oidc")}},
			wantRules: []string{RuleClassificationVocabulary},
		},
		{
			name:      "public route without auth",
			change:    Change{Proposed: []resource.Resource{route("app", "family_public", "none")}},
			wantRules: []string{RulePublicRequiresAuth},
		},
		{
			name: "downgrade laundering: currently public, reclassified and auth dropped in one change",
			change: Change{
				Proposed: []resource.Resource{route("app", "internal_only", "none")},
				Current:  []resource.Resource{route("app", "family_public", "authelia_oidc")},
			},
			wantRules: []string{RulePublicRequiresAuth},
		},
		{
			name: "internal route without auth is fine",
			change: Change{
				Proposed: []resource.Resource{route("app", "internal_only", "none")},
			},
		},
		{
			name: "admin route migrated to public ingress",
			change: Change{
				Proposed: []resource.Resource{route("console", "family_public", "authelia_oidc")},
				Current:  []resource.Resource{route("console", "admin_tailscale", "authelia_oidc")},
			},
			wantRules: []string{RuleAdminStructural},
		},
		{
			name: "admin route to internal_only is allowed (decommission path)",
			change: Change{
				Proposed: []resource.Resource{route("console", "internal_only", "authelia_oidc")},
				Current:  []resource.Resource{route("console", "admin_tailscale", "authelia_oidc")},
			},
		},
		{
			name: "credential-shaped env key with literal value",
			change: Change{
				Proposed: []resource.Resource{container("app", map[string]string{"DB_PASSWORD": "hunter2"})},
			},
			wantRules: []string{RuleSecretValuesForbidden},
		},
		{
			name: "credential-shaped env key with secret reference is fine",
			change: Change{
				Proposed: []resource.Resource{container("app", map[string]string{"DB_PASSWORD": "secret/app-db"})},
			},
		},
		{
			name: "secret-shaped value under an innocent key",
			change: Change{
				Proposed: []resource.Resource{container("app", map[string]string{
					"STARTUP_FLAG": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
				})},
			},
			wantRules: []string{RuleSecretValuesForbidden},
		},
		{
			name: "digest-pinned image value under an innocent key is fine",
			change: Change{
				Proposed: []resource.Resource{container("app", map[string]string{
					"SIDECAR_IMAGE": "reg.example/x@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				})},
			},
		},
		{
			name: "butler may not modify an admin route",
			change: Change{
				Proposed: []resource.Resource{route("console", "admin_tailscale", "none")},
				Current:  []resource.Resource{route("console", "admin_tailscale", "authelia_oidc")},
				Role:     "butler",
			},
			wantRules: []string{RuleButlerAdminScope},
		},
		{
			name: "butler may not delete an admin route",
			change: Change{
				Proposed: nil,
				Current:  []resource.Resource{route("console", "admin_tailscale", "authelia_oidc")},
				Role:     "butler",
			},
			wantRules: []string{RuleButlerAdminScope},
		},
		{
			name: "operator may modify an admin route",
			change: Change{
				Proposed: []resource.Resource{route("console", "admin_tailscale", "none")},
				Current:  []resource.Resource{route("console", "admin_tailscale", "authelia_oidc")},
				Role:     "operator",
			},
		},
		{
			name: "butler leaving an admin route untouched is fine",
			change: Change{
				Proposed: []resource.Resource{
					route("console", "admin_tailscale", "authelia_oidc"),
					route("app", "family_public", "authelia_oidc"),
				},
				Current: []resource.Resource{route("console", "admin_tailscale", "authelia_oidc")},
				Role:    "butler",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			violations, exempted := e.Validate(c.change)
			if len(exempted) != 0 {
				t.Fatalf("no exemptions configured but got %v", exempted)
			}
			got := ruleIDs(violations)
			if len(got) != len(c.wantRules) {
				t.Fatalf("violations = %v, want rules %v", violations, c.wantRules)
			}
			for i := range got {
				if got[i] != c.wantRules[i] {
					t.Fatalf("violations = %v, want rules %v", violations, c.wantRules)
				}
			}
		})
	}
}

func TestExemptionSuppressesAndSurfaces(t *testing.T) {
	ex := Exemption{
		Rule: RulePublicRequiresAuth, Kind: "Route", Name: "legacy-cam",
		Reason: "camera firmware cannot speak OIDC; isolated VLAN", ApprovedBy: "mika",
	}
	if err := ex.Validate(); err != nil {
		t.Fatal(err)
	}
	e := NewEngine([]Exemption{ex})
	violations, exempted := e.Validate(Change{
		Proposed: []resource.Resource{
			route("legacy-cam", "family_public", "none"),
			route("other", "family_public", "none"),
		},
	})
	if len(exempted) != 1 || exempted[0].Name != "legacy-cam" {
		t.Fatalf("exempted = %v", exempted)
	}
	if len(violations) != 1 || violations[0].Name != "other" {
		t.Fatalf("violations = %v (exemption must be per-name, not per-rule)", violations)
	}
}

func TestExemptionValidation(t *testing.T) {
	for name, ex := range map[string]Exemption{
		"unknown rule":  {Rule: "nope", Kind: "Route", Name: "x", Reason: "r", ApprovedBy: "m"},
		"missing name":  {Rule: RulePublicRequiresAuth, Kind: "Route", Reason: "r", ApprovedBy: "m"},
		"missing why":   {Rule: RulePublicRequiresAuth, Kind: "Route", Name: "x", ApprovedBy: "m"},
		"missing whoby": {Rule: RulePublicRequiresAuth, Kind: "Route", Name: "x", Reason: "r"},
	} {
		if err := ex.Validate(); err == nil {
			t.Errorf("%s: Validate() passed, want error", name)
		}
	}
}

func TestLoadExemptions(t *testing.T) {
	dir := t.TempDir()
	// Missing file: no exemptions, no error.
	got, err := LoadExemptions(filepath.Join(dir, "absent.yaml"))
	if err != nil || got != nil {
		t.Fatalf("missing file = %v, %v", got, err)
	}
	path := filepath.Join(dir, "policy-exemptions.yaml")
	body := `
- rule: public-requires-auth
  kind: Route
  name: legacy-cam
  reason: camera firmware cannot speak OIDC
  approvedBy: mika
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadExemptions(path)
	if err != nil || len(got) != 1 || got[0].Name != "legacy-cam" {
		t.Fatalf("LoadExemptions = %+v, %v", got, err)
	}
	// A malformed entry fails the whole load, loudly.
	if err := os.WriteFile(path, []byte("- rule: nope\n  kind: Route\n  name: x\n  reason: r\n  approvedBy: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExemptions(path); err == nil || !strings.Contains(err.Error(), "unknown rule") {
		t.Fatalf("bad exemption load = %v, want unknown-rule error", err)
	}
}

func TestTiers(t *testing.T) {
	for _, tier := range []Tier{TierLow, TierMedium, TierHigh, TierDomestic} {
		if !ValidTier(tier) {
			t.Errorf("ValidTier(%s) = false", tier)
		}
	}
	if ValidTier("critical") {
		t.Error("unknown tier validated")
	}
	if RequiresApproval(TierLow) {
		t.Error("low must auto-apply (ADR-W004 A3)")
	}
	for _, tier := range []Tier{TierMedium, TierHigh, TierDomestic, Tier("unknown")} {
		if !RequiresApproval(tier) {
			t.Errorf("RequiresApproval(%s) = false, want fail-safe true", tier)
		}
	}
}
