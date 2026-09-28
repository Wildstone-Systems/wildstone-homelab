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

func automation(name, tier string, params map[string]string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindAutomation,
		Metadata:   resource.Metadata{Name: name, Owner: "household"},
		Automation: &resource.AutomationSpec{
			Description: "test rule",
			Trigger:     resource.AutomationTrigger{EventType: "person.left_home"},
			Action:      resource.AutomationAction{Tool: "notifications.notify", Tier: tier, Params: params},
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
			name:   "household rule with a domestic action",
			change: Change{Proposed: []resource.Resource{automation("breakfast", "domestic", nil)}, Role: "butler"},
		},
		{
			name:   "household rule with a read action",
			change: Change{Proposed: []resource.Resource{automation("breakfast", "read", nil)}},
		},
		{
			name:      "household rule with an infrastructure-tier action, from the butler",
			change:    Change{Proposed: []resource.Resource{automation("restart-things", "high", nil)}, Role: "butler"},
			wantRules: []string{RuleAutomationActionTier},
		},
		{
			name:      "household rule with an infrastructure-tier action, from an operator, is refused the same",
			change:    Change{Proposed: []resource.Resource{automation("restart-things", "medium", nil)}, Role: "operator"},
			wantRules: []string{RuleAutomationActionTier},
		},
		{
			name: "household rule carrying a literal secret in a param",
			change: Change{Proposed: []resource.Resource{automation("leaky", "domestic",
				map[string]string{"api_key": "hunter2"})}},
			wantRules: []string{RuleSecretValuesForbidden},
		},
		{
			name: "household rule referencing a secret by name in a param is fine",
			change: Change{Proposed: []resource.Resource{automation("clean", "domestic",
				map[string]string{"api_key": "secret/ntfy-token"})}},
		},
		{
			name: "untouched household rule with a bad tier is legacy drift, not a block",
			change: Change{
				Proposed: []resource.Resource{automation("old", "high", nil), route("app", "family_public", "authelia_oidc")},
				Current:  []resource.Resource{automation("old", "high", nil)},
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

// job mirrors container(), for the kind that also runs one.
func job(name string, env map[string]string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindJob,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Job: &resource.JobSpec{Run: resource.ContainerSpec{
			Image: "reg.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Env:   env,
		}},
	}
}

// TestJobEnvIsHeldToTheContainerSecretRule closes the hole the Job kind
// opened. The rule read r.Container.Env, so a Job's env — which is
// EXACTLY where a literal would appear, since a provisioning job needs
// an admin password to do its work — was checked by nothing.
//
// A container and a job with identical env must produce identical
// violations; anything else means one of the two kinds is unguarded.
func TestJobEnvIsHeldToTheContainerSecretRule(t *testing.T) {
	e := NewEngine(nil)
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"literal in a credential-shaped key", map[string]string{"LLDAP_ADMIN_PASSWORD": "hunter2"}, []string{RuleSecretValuesForbidden}},
		{"a proper reference", map[string]string{"LLDAP_ADMIN_PASSWORD": "secret/lldap-admin-password"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asContainer := func() []string {
				v, _ := e.Validate(Change{Proposed: []resource.Resource{container("x", tc.env)}})
				return ruleIDs(v)
			}()
			asJob := func() []string {
				v, _ := e.Validate(Change{Proposed: []resource.Resource{job("x", tc.env)}})
				return ruleIDs(v)
			}()

			if strings.Join(asJob, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Job env %v produced %v, want %v", tc.env, asJob, tc.want)
			}
			if strings.Join(asJob, ",") != strings.Join(asContainer, ",") {
				t.Errorf("the same env is judged differently by kind: Container %v, Job %v — one of the "+
					"two is not being checked", asContainer, asJob)
			}
		})
	}
}

func ldapUser(name, node, email string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindLdapUser,
		Metadata:   resource.Metadata{Name: name, Owner: "household", Node: node},
		LdapUser:   &resource.LdapUserSpec{Username: name, Email: email},
	}
}

// Two members of one directory cannot share an email. LLDAP refuses the
// second createUser on its unique index, and the provisioning job that
// hits it fails every later plan touching the directory.
func TestLdapUserEmailUnique(t *testing.T) {
	e := NewEngine(nil)
	sam := ldapUser("sam", "n1", "sam@example.com")
	cases := []struct {
		name   string
		change Change
		want   []string
	}{
		{
			name: "a new member with another member's email",
			change: Change{
				Proposed: []resource.Resource{sam, ldapUser("kim", "n1", "sam@example.com")},
				Current:  []resource.Resource{sam},
			},
			want: []string{RuleLdapUserEmailUnique},
		},
		{
			name: "case is not a different address to the directory",
			change: Change{
				Proposed: []resource.Resource{sam, ldapUser("kim", "n1", "Sam@Example.com")},
				Current:  []resource.Resource{sam},
			},
			want: []string{RuleLdapUserEmailUnique},
		},
		{
			name: "changing a member's email onto another's",
			change: Change{
				Proposed: []resource.Resource{sam, ldapUser("kim", "n1", "sam@example.com")},
				Current:  []resource.Resource{sam, ldapUser("kim", "n1", "kim@example.com")},
			},
			want: []string{RuleLdapUserEmailUnique},
		},
		{
			name: "distinct emails",
			change: Change{
				Proposed: []resource.Resource{sam, ldapUser("kim", "n1", "kim@example.com")},
				Current:  []resource.Resource{sam},
			},
		},
		{
			name: "the same email on another directory is another account",
			change: Change{
				Proposed: []resource.Resource{sam, ldapUser("sam2", "n2", "sam@example.com")},
				Current:  []resource.Resource{sam},
			},
		},
		{
			name: "an existing duplicate does not wedge an unrelated change",
			change: Change{
				Proposed: []resource.Resource{sam, ldapUser("kim", "n1", "sam@example.com"), route("app", "internal_only", "none")},
				Current:  []resource.Resource{sam, ldapUser("kim", "n1", "sam@example.com")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := e.Validate(tc.change)
			if strings.Join(ruleIDs(got), ",") != strings.Join(tc.want, ",") {
				t.Fatalf("violations = %v, want %v", got, tc.want)
			}
			for _, v := range got {
				if v.Name != "kim" || !strings.Contains(v.Msg, "sam") {
					t.Fatalf("violation %v should be on kim and name sam", v)
				}
			}
		})
	}
}

func backupTarget(mut func(*resource.BackupTargetSpec)) resource.Resource {
	r := resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindBackupTarget,
		Metadata:   resource.Metadata{Name: "offsite", Owner: "owner", Node: "n1"},
		BackupTarget: &resource.BackupTargetSpec{
			Type: "s3", Endpoint: "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com",
			Bucket: "wildstone-backups", Prefix: "home", Region: "auto",
			AccessKeySecret: "secret/offsite-access", SecretKeySecret: "secret/offsite-secret",
		},
	}
	if mut != nil {
		mut(r.BackupTarget)
	}
	return r
}

// TestBackupTargetSecretShapedFields: a credential pasted into a plain
// field is refused; a real R2 endpoint, whose account ID looks like a
// token, is not.
func TestBackupTargetSecretShapedFields(t *testing.T) {
	e := NewEngine(nil)
	if v, _ := e.Validate(Change{Proposed: []resource.Resource{backupTarget(nil)}}); len(v) != 0 {
		t.Fatalf("clean R2 target flagged: %v", v)
	}
	for field, mut := range map[string]func(*resource.BackupTargetSpec){
		"prefix": func(s *resource.BackupTargetSpec) { s.Prefix = "REPLACED-" + strings.Repeat("k7Qz", 8) },
		"bucket": func(s *resource.BackupTargetSpec) { s.Bucket = "0123456789abcdef0123456789abcdef" },
	} {
		v, _ := e.Validate(Change{Proposed: []resource.Resource{backupTarget(mut)}})
		if len(v) != 1 || v[0].Rule != RuleSecretValuesForbidden || !strings.Contains(v[0].Msg, field) {
			t.Errorf("%s: violations = %v, want one %s naming the field", field, v, RuleSecretValuesForbidden)
		}
	}
}

func offsitePlan(node, target string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindBackupPlan,
		Metadata:   resource.Metadata{Name: "photos", Owner: "capability/photos", Node: node},
		BackupPlan: &resource.BackupPlanSpec{
			Volumes: []string{"immich-library"}, Repository: "backup-repo",
			PasswordSecret: "secret/restic-photos", Schedule: "daily",
			Offsite: &resource.BackupOffsite{Target: target, Route: resource.BackupRouteLocalAndOffsite,
				PasswordSecret: "secret/restic-photos-offsite", Schedule: "weekly"},
		},
	}
}

// TestOffsiteTargetDeclared: a plan's off-site target must exist on the
// plan's node, and cannot be removed from under a plan still using it.
func TestOffsiteTargetDeclared(t *testing.T) {
	e := NewEngine(nil)
	tgt := backupTarget(nil) // node n1
	moved := backupTarget(nil)
	moved.Metadata.Node = "n2"
	rebucketed := backupTarget(func(s *resource.BackupTargetSpec) { s.Bucket = "other-bucket" })
	for _, tc := range []struct {
		name     string
		ch       Change
		wantRule bool
	}{
		{"target present", Change{Proposed: []resource.Resource{tgt, offsitePlan("n1", "offsite")}}, false},
		{"target missing", Change{Proposed: []resource.Resource{offsitePlan("n1", "offsite")}}, true},
		{"target on another node", Change{Proposed: []resource.Resource{tgt, offsitePlan("n2", "offsite")}}, true},
		{"target deleted under an untouched plan", Change{
			Current:  []resource.Resource{tgt, offsitePlan("n1", "offsite")},
			Proposed: []resource.Resource{offsitePlan("n1", "offsite")},
		}, true},
		{"target moved to another node under an untouched plan", Change{
			Current:  []resource.Resource{tgt, offsitePlan("n1", "offsite")},
			Proposed: []resource.Resource{moved, offsitePlan("n1", "offsite")},
		}, true},
		{"target edited in place under an untouched plan", Change{
			Current:  []resource.Resource{tgt, offsitePlan("n1", "offsite")},
			Proposed: []resource.Resource{rebucketed, offsitePlan("n1", "offsite")},
		}, false},
		{"unrelated change with target intact", Change{
			Current:  []resource.Resource{tgt, offsitePlan("n1", "offsite")},
			Proposed: []resource.Resource{tgt, offsitePlan("n1", "offsite"), container("app", nil)},
		}, false},
	} {
		v, _ := e.Validate(tc.ch)
		got := false
		for _, x := range v {
			got = got || x.Rule == RuleBackupTargetDeclared
		}
		if got != tc.wantRule {
			t.Errorf("%s: violations = %v, want %s: %v", tc.name, v, RuleBackupTargetDeclared, tc.wantRule)
		}
	}
}
