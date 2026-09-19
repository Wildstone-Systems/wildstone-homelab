package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// route, group and the fixtures the enforcement tests share.
func route(name, capability, host, auth string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindRoute,
		Metadata: resource.Metadata{Name: name, Owner: "capability/" + capability, Node: "n1"},
		Route: &resource.RouteSpec{
			Host: host, Upstream: "container/x:80",
			Auth: auth, Classification: "family_public",
		},
	}
}

func ldapGroup(name, capability, group string) resource.Resource {
	return resource.Resource{
		Kind:      resource.KindLdapGroup,
		Metadata:  resource.Metadata{Name: name, Owner: "capability/" + capability, Node: "n1"},
		LdapGroup: &resource.LdapGroupSpec{Name: group},
	}
}

// THE central negative case (#209): a direct URL to an unauthorized
// service is denied.
//
// Asserted at the AUTHORIZATION layer, as the issue demands — the
// rendered Authelia policy, which is what decides a forward-auth
// request — and not by checking that a Homepage card is hidden.
//
// Named honestly about what it can reach: Keystone does not run Authelia
// in CI, so this proves the policy Wildstone writes denies a person
// outside the group, not that a live Authelia then refuses the request.
// Authelia's own behaviour for `subject: group:x` with default_policy
// deny is its documented contract; what Wildstone can get wrong, and
// what this catches, is failing to write the subject at all — which is
// exactly how every forward-auth app came to admit any household member.
func TestDirectURLToUnauthorizedServiceIsDenied(t *testing.T) {
	desired := []resource.Resource{
		ldapGroup("docs-users", "documents", "docs_users"),
		route("docs", "documents", "docs.example.com", "authelia_oidc"),
	}
	out := string(renderAutheliaAccess(desired))

	if !strings.Contains(out, "default_policy: deny") {
		t.Fatalf("the policy does not default to deny:\n%s", out)
	}
	if !strings.Contains(out, "subject:") || !strings.Contains(out, "- 'group:docs_users'") {
		t.Errorf("the rule carries no subject, so any household member Authelia "+
			"admits reaches the app — access would be whatever the directory "+
			"happens to say, which is the state §4 exists to end:\n%s", out)
	}
}

// Homepage visibility is presentation only (#209).
//
// The same point from the other side, and it is a STRUCTURAL claim: the
// authorization answer must not be reachable from what a dashboard
// renders. Authelia decides a forward-auth request from access.yml
// alone, and nothing about a card can appear in it — so a visible card
// for an unentitled service still meets a policy that denies.
func TestHomepageVisibilityDoesNotGrantAccess(t *testing.T) {
	// A Homepage that lists the service, beside the service itself.
	desired := []resource.Resource{
		ldapGroup("docs-users", "documents", "docs_users"),
		route("docs", "documents", "docs.example.com", "authelia_oidc"),
		route("home", "dashboard", "home.example.com", "portal"),
	}
	out := string(renderAutheliaAccess(desired))

	// The rule that decides docs.example.com is unchanged by anything
	// the dashboard does.
	if !strings.Contains(out, "- 'group:docs_users'") {
		t.Errorf("the docs rule lost its subject when a dashboard was present:\n%s", out)
	}
	// And the Homepage's own Route does not inherit the docs subject:
	// they are separate hosts with separate rules.
	rules := strings.Split(out, "- domain: ")
	for _, r := range rules {
		if strings.HasPrefix(r, "home.example.com") && strings.Contains(r, "group:docs_users") {
			t.Errorf("the dashboard's rule carries the docs subject:\n%s", out)
		}
	}
}

// Silent where nothing was declared.
//
// This is the restraint that makes the feature safe to ship to a running
// household. A subject list on a host whose group is empty denies
// EVERYONE, so a capability that declared no directory groups keeps the
// behaviour it has today rather than being locked shut by an upgrade
// that added a field.
func TestAHostWithNoDeclaredGroupsKeepsItsCurrentBehaviour(t *testing.T) {
	// A deployment where SOMETHING declares groups, which is the only
	// interesting case: with nothing declared anywhere the renderer
	// short-circuits and the branch under test never runs. The first
	// version of this test had exactly that fixture and passed against
	// an implementation that gave every undeclared host a subject.
	desired := []resource.Resource{
		ldapGroup("docs-users", "documents", "docs_users"),
		route("docs", "documents", "docs.example.com", "authelia_oidc"),
		route("feeds", "feeds", "feeds.example.com", "authelia_oidc"),
	}
	out := string(renderAutheliaAccess(desired))

	feeds := ruleFor(t, out, "feeds.example.com")
	if strings.Contains(feeds, "subject:") {
		t.Errorf("a capability that declared no groups was given a subject, which "+
			"denies everyone:\n%s", out)
	}
	if !strings.Contains(feeds, "policy:") {
		t.Errorf("the rule lost its policy:\n%s", out)
	}
	// And the one that DID declare still has its subject, or this test
	// would pass against a renderer that emitted none at all.
	if !strings.Contains(ruleFor(t, out, "docs.example.com"), "group:docs_users") {
		t.Errorf("the declared host lost its subject:\n%s", out)
	}
}

// ruleFor returns the block of access.yml governing one hostname.
func ruleFor(t *testing.T, rendered, domain string) string {
	t.Helper()
	for _, block := range strings.Split(rendered, "    - domain: ") {
		if strings.HasPrefix(block, domain+"\n") {
			return block
		}
	}
	t.Fatalf("no rule for %s in:\n%s", domain, rendered)
	return ""
}

// Only forward-auth gets a subject.
//
// app_oidc is gated by the OIDC client's own authorization policy,
// app_ldap by the app's search filter, and portal bypasses Authelia
// because Keystone decides it in-process. A subject on any of those
// would be enforcement in the wrong place, or none at all.
func TestOnlyForwardAuthRoutesCarryASubject(t *testing.T) {
	for _, auth := range []string{"app_oidc", "app_ldap", "none"} {
		desired := []resource.Resource{
			ldapGroup("photos-users", "photos", "photos_users"),
			route("photos", "photos", "photos.example.com", auth),
		}
		if subjects := AccessSubjects(desired); len(subjects) != 0 {
			t.Errorf("auth %q was given subjects %v; it is not gated by the "+
				"forward-auth rules", auth, subjects)
		}
	}
	// portal produces a rule (bypass) but must carry no subject: it is
	// Keystone's own surface, refused in-process against Remote-Groups.
	desired := []resource.Resource{
		ldapGroup("console-users", "console", "console_users"),
		route("console", "console", "console.example.com", "portal"),
	}
	out := string(renderAutheliaAccess(desired))
	if strings.Contains(out, "subject:") {
		t.Errorf("a portal route carries a subject; bypass plus a subject is a "+
			"contradiction, and the gate is Keystone's:\n%s", out)
	}
}

// Two services on one hostname admit the union of their audiences.
//
// Under the tailnet posture every Route shares one hostname, so the
// rules merge. An intersection would deny somebody entitled to one of
// the two services on the strength of the other, which is not what
// either entitlement said.
func TestServicesSharingAHostnameAdmitTheUnion(t *testing.T) {
	desired := []resource.Resource{
		ldapGroup("docs-users", "documents", "docs_users"),
		ldapGroup("photos-users", "photos", "photos_users"),
		route("docs", "documents", "shared.example.com", "authelia_oidc"),
		route("photos", "photos", "shared.example.com", "authelia_oidc"),
	}
	got := AccessSubjects(desired)["shared.example.com"]
	if len(got) != 2 || got[0] != "docs_users" || got[1] != "photos_users" {
		t.Errorf("subjects = %v, want both groups sorted — each Route's capability "+
			"admits its own people", got)
	}
}

// The file is a pure function of desired state.
//
// An unstable order would rewrite access.yml and restart Authelia on
// ticks where nothing changed, which is a household losing its sessions
// every reconcile.
func TestSubjectsAreStableAcrossRenders(t *testing.T) {
	desired := []resource.Resource{
		ldapGroup("b-users", "bee", "b_users"),
		ldapGroup("a-users", "ay", "a_users"),
		route("b", "bee", "shared.example.com", "authelia_oidc"),
		route("a", "ay", "shared.example.com", "authelia_oidc"),
	}
	first := string(renderAutheliaAccess(desired))
	for i := 0; i < 5; i++ {
		if got := string(renderAutheliaAccess(desired)); got != first {
			t.Fatalf("render %d differs:\n%s\n---\n%s", i, first, got)
		}
	}
}

// §4's "OIDC where supported": an app that runs its own login flow is
// gated by the token the IdP will not issue.
//
// RequireGroups has been implementable since it was written and no
// shipped package set it, so every app_oidc service admitted any
// household member the policy allowed. The capability's declared groups
// are the answer.
func TestAnOidcClientIsGatedByItsCapabilitysGroups(t *testing.T) {
	desired := []resource.Resource{
		ldapGroup("photos-users", "photos", "photos_users"),
		{
			Kind:     resource.KindOidcClient,
			Metadata: resource.Metadata{Name: "immich", Owner: "capability/photos", Node: "n1"},
			OidcClient: &resource.OidcClientSpec{
				ClientId: "immich", ClientName: "Immich",
				RedirectUris: []string{"https://photos.example.com/auth/login"},
				Scopes:       []string{"openid"}, AuthPolicy: "two_factor",
				SecretRef: "secret/photos-oidc",
			},
		},
	}
	out := string(renderAutheliaClients(desired))
	if !strings.Contains(out, "authorization_policies:") {
		t.Fatalf("no per-client policy was rendered, so the client admits anyone "+
			"the factor admits:\n%s", out)
	}
	if !strings.Contains(out, "- 'group:photos_users'") {
		t.Errorf("the client is not restricted to its capability's group:\n%s", out)
	}
	if !strings.Contains(out, "default_policy: 'deny'") {
		t.Errorf("the per-client policy does not default to deny:\n%s", out)
	}
}

// A package that named its own groups keeps them.
//
// The package said something more specific than its capability did, and
// a derivation that overruled it would silently widen or narrow what an
// author chose deliberately.
func TestAnOidcClientsOwnGroupsAreNotOverruled(t *testing.T) {
	desired := []resource.Resource{
		ldapGroup("photos-users", "photos", "photos_users"),
		{
			Kind:     resource.KindOidcClient,
			Metadata: resource.Metadata{Name: "immich", Owner: "capability/photos", Node: "n1"},
			OidcClient: &resource.OidcClientSpec{
				ClientId: "immich", ClientName: "Immich",
				RedirectUris: []string{"https://photos.example.com/auth/login"},
				Scopes:       []string{"openid"}, AuthPolicy: "two_factor",
				SecretRef: "secret/photos-oidc", RequireGroups: []string{"photos_admins"},
			},
		},
	}
	out := string(renderAutheliaClients(desired))
	if !strings.Contains(out, "- 'group:photos_admins'") {
		t.Errorf("the package's own audience was replaced:\n%s", out)
	}
	if strings.Contains(out, "- 'group:photos_users'") {
		t.Errorf("the capability's groups were added beside the package's:\n%s", out)
	}
}

// And a capability that declared nothing gates nothing.
//
// The same restraint as the access rules: a gate over an empty group
// refuses everyone, so an upgrade that added a field must not close an
// app nobody decided to close.
func TestAnOidcClientWithNoDeclaredGroupsIsUngated(t *testing.T) {
	desired := []resource.Resource{
		ldapGroup("photos-users", "photos", "photos_users"),
		{
			Kind:     resource.KindOidcClient,
			Metadata: resource.Metadata{Name: "actual", Owner: "capability/budget", Node: "n1"},
			OidcClient: &resource.OidcClientSpec{
				ClientId: "actual", ClientName: "Actual",
				RedirectUris: []string{"https://budget.example.com/cb"},
				Scopes:       []string{"openid"}, AuthPolicy: "two_factor",
				SecretRef: "secret/budget-oidc",
			},
		},
	}
	out := string(renderAutheliaClients(desired))
	if strings.Contains(out, "authorization_policy: 'policy_actual'") {
		t.Errorf("a client whose capability declared no groups was gated, which "+
			"denies everyone:\n%s", out)
	}
}
