package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func oidcClient(name string, mutate func(*resource.OidcClientSpec)) resource.Resource {
	spec := &resource.OidcClientSpec{
		ClientId:     name,
		ClientName:   strings.ToUpper(name[:1]) + name[1:],
		RedirectUris: []string{"https://" + name + ".example.com/cb"},
		Scopes:       []string{"openid", "profile", "email"},
		AuthPolicy:   "one_factor",
		SecretRef:    "secret/" + name + "-oidc",
	}
	if mutate != nil {
		mutate(spec)
	}
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindOidcClient,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/" + name, Node: "node-a"},
		OidcClient: spec,
	}
}

// TestEveryClientSurvivesTheMerge is the finding this file exists for.
// Authelia does not union config files: several files that each define
// identity_providers.oidc.clients leave only the last list standing. One
// file per client therefore registered exactly one client and reported
// nothing — the second app's login failed with "client does not exist"
// while its config file sat on disk looking correct.
func TestEveryClientSurvivesTheMerge(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("immich", nil),
		oidcClient("paperless", nil),
		oidcClient("jellyfin", nil),
	}))
	if n := strings.Count(got, "clients:"); n != 1 {
		t.Fatalf("rendered %d clients: keys, want exactly 1 — a second one replaces the first:\n%s", n, got)
	}
	for _, want := range []string{"'immich'", "'paperless'", "'jellyfin'"} {
		if !strings.Contains(got, "client_id: "+want) {
			t.Errorf("client %s missing from the merged list:\n%s", want, got)
		}
	}
}

// Byte-comparison decides drift, so input order must not change output.
func TestMergedClientsAreOrderIndependent(t *testing.T) {
	a := renderAutheliaClients([]resource.Resource{oidcClient("immich", nil), oidcClient("paperless", nil)})
	b := renderAutheliaClients([]resource.Resource{oidcClient("paperless", nil), oidcClient("immich", nil)})
	if string(a) != string(b) {
		t.Fatalf("render is order-dependent, so an unchanged deployment would look drifted:\n%s\n---\n%s", a, b)
	}
}

// Authelia refuses to start with an empty clients list, so a node with no
// OidcClient must get a file with no clients key rather than an empty
// one. An identity provider that will not boot is worse than one serving
// no clients: it takes every login with it, including the operator's.
func TestNoClientsRendersNoEmptyList(t *testing.T) {
	got := string(renderAutheliaClients(nil))
	if strings.Contains(got, "clients:") {
		t.Fatalf("empty deployment rendered a clients key:\n%s", got)
	}
	if !strings.Contains(got, "identity_providers:") {
		t.Fatalf("rendered file is not a valid Authelia config shape:\n%s", got)
	}
}

// Two first-party apps need opposite values here and both are correct, so
// there is no default the renderer could bake in. Immich sends its secret
// in the POST body and fails against Authelia's default with
// invalid_client; Paperless uses the default.
func TestPerClientAuthMethodAndPkce(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("immich", func(s *resource.OidcClientSpec) {
			s.TokenAuthMethod = "client_secret_post"
			s.RequirePkce = true
		}),
		oidcClient("paperless", nil),
	}))
	immich := clientBlock(t, got, "immich")
	paperless := clientBlock(t, got, "paperless")

	if !strings.Contains(immich, "token_endpoint_auth_method: 'client_secret_post'") {
		t.Errorf("immich did not get client_secret_post:\n%s", immich)
	}
	if !strings.Contains(immich, "require_pkce: true") || !strings.Contains(immich, "pkce_challenge_method: 'S256'") {
		t.Errorf("immich did not get PKCE S256:\n%s", immich)
	}
	// The default is emitted rather than omitted: a client that POSTs its
	// credentials against an unstated default fails with an error naming
	// the method, which reads as the client being wrong.
	if !strings.Contains(paperless, "token_endpoint_auth_method: 'client_secret_basic'") {
		t.Errorf("paperless did not get the explicit default:\n%s", paperless)
	}
	if strings.Contains(paperless, "require_pkce") {
		t.Errorf("paperless got PKCE it did not ask for:\n%s", paperless)
	}
}

// For an app that discards the groups claim — Paperless has no OIDC group
// mapping at all — the IdP refusing the token is the only gate that
// exists. A user it admits instead lands with zero permissions, which
// looks like a broken install rather than a refusal.
func TestRequireGroupsBecomesADenyByDefaultPolicy(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("paperless", func(s *resource.OidcClientSpec) {
			s.RequireGroups = []string{"docs_users", "admins"}
		}),
		oidcClient("immich", nil),
	}))
	if !strings.Contains(got, "authorization_policies:") {
		t.Fatalf("no authorization_policies block:\n%s", got)
	}
	if !strings.Contains(got, "default_policy: 'deny'") {
		t.Errorf("gated policy does not deny by default:\n%s", got)
	}
	for _, g := range []string{"'group:docs_users'", "'group:admins'"} {
		if !strings.Contains(got, g) {
			t.Errorf("subject %s missing:\n%s", g, got)
		}
	}
	if !strings.Contains(clientBlock(t, got, "paperless"), "authorization_policy: 'paperless_groups'") {
		t.Errorf("gated client does not reference its policy:\n%s", got)
	}
	// An ungated client must keep its bare policy, not inherit a gate.
	if !strings.Contains(clientBlock(t, got, "immich"), "authorization_policy: 'one_factor'") {
		t.Errorf("ungated client lost its policy:\n%s", got)
	}
}

// "admit these groups, then deny them" is not a thing anyone means.
func TestDenyPlusRequireGroupsStillAdmitsTheGroups(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("locked", func(s *resource.OidcClientSpec) {
			s.AuthPolicy = "deny"
			s.RequireGroups = []string{"admins"}
		}),
	}))
	if !strings.Contains(got, "- policy: 'one_factor'") {
		t.Errorf("the admit-rule denies, so the policy admits nobody:\n%s", got)
	}
}

// A scope alone puts claims at /userinfo. An app reading groups from the
// ID token sees nothing without a claims policy.
func TestIdTokenClaimsBecomeAClaimsPolicy(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("immich", func(s *resource.OidcClientSpec) {
			s.IdTokenClaims = []string{"groups", "email"}
		}),
		oidcClient("plain", nil),
	}))
	if !strings.Contains(got, "claims_policies:") || !strings.Contains(got, "immich_claims:") {
		t.Fatalf("no claims policy for a client that asked for ID-token claims:\n%s", got)
	}
	if !strings.Contains(clientBlock(t, got, "immich"), "claims_policy: 'immich_claims'") {
		t.Errorf("client does not reference its claims policy:\n%s", got)
	}
	if strings.Contains(clientBlock(t, got, "plain"), "claims_policy:") {
		t.Errorf("client that asked for nothing got a claims policy:\n%s", got)
	}
}

// The renderer must never emit a resolved secret (ADR-W005 A2).
func TestMergedClientsCarryOnlySecretReferences(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{oidcClient("immich", nil)}))
	if !strings.Contains(got, "DIGEST-PLACEHOLDER:secret/immich-oidc") {
		t.Fatalf("secret reference not carried as a placeholder:\n%s", got)
	}
	if strings.Contains(got, "$pbkdf2") {
		t.Fatalf("rendered file contains something shaped like a real hashed secret:\n%s", got)
	}
}

// Non-OidcClient resources are ignored rather than crashing the render,
// since prereq repair hands it the whole desired set for the node.
func TestNonOidcResourcesAreIgnored(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		{Kind: resource.KindVolume, Metadata: resource.Metadata{Name: "data", Owner: "system", Node: "node-a"},
			Volume: &resource.VolumeSpec{Name: "data"}},
		oidcClient("immich", nil),
	}))
	if !strings.Contains(got, "client_id: 'immich'") || strings.Contains(got, "data") {
		t.Fatalf("unexpected render:\n%s", got)
	}
}

// clientBlock extracts one client's rendered lines, so an assertion about
// client A cannot be satisfied by client B's text.
func clientBlock(t *testing.T, rendered, clientID string) string {
	t.Helper()
	start := strings.Index(rendered, "      - client_id: '"+clientID+"'")
	if start < 0 {
		t.Fatalf("client %s not found in:\n%s", clientID, rendered)
	}
	rest := rendered[start+1:]
	if end := strings.Index(rest, "      - client_id: "); end >= 0 {
		return rendered[start : start+1+end]
	}
	return rendered[start:]
}
