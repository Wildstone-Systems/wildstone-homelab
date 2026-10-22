package reconcile

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
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

// The renderer must never emit a resolved secret (ADR-W005 A2). What it
// emits is a reference marked for digest materialization, which the node
// resolves when it writes the file — Keystone holds no secret values and
// so could not produce the digest even if it wanted to.
func TestMergedClientsCarryOnlySecretReferences(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{oidcClient("immich", nil)}))
	if !strings.Contains(got, "client_secret: 'pbkdf2:secret/immich-oidc'") {
		t.Fatalf("secret not carried as a digest-materialized reference:\n%s", got)
	}
	// The rendered form must be nothing a provider could mistake for a
	// real digest: if this ever contains one, a secret reached Keystone.
	if strings.Contains(got, "$pbkdf2-sha512$") {
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

// TestRenderedClientsCarryTheProviderKeys covers the two things an OIDC
// provider cannot serve without, and which this renderer emitted
// neither of until now. The identity package's comment claimed
// clients.yml carried both; it did not, and Authelia would have refused
// to start on the first app installed.
func TestRenderedClientsCarryTheProviderKeys(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClientResource("photos", "photos", "secret/photos-oidc"),
	}))

	if !strings.Contains(got, "    hmac_secret: "+autheliaHmacSecretRef+"\n") {
		t.Errorf("no hmac_secret in the rendered provider block:\n%s", got)
	}
	// The jwks key sits inside a YAML block scalar, and the PEM that
	// replaces the reference is many lines. The agent indents every
	// continuation line to the column the reference starts at, so that
	// column is load-bearing: get it wrong and the substituted key
	// breaks the document.
	wantJwks := "    jwks:\n      - key: |\n          " + autheliaJwksSecretRef + "\n"
	if !strings.Contains(got, wantJwks) {
		t.Errorf("the jwks block is not in the shape the materializer indents against.\ngot:\n%s\nwant to contain:\n%s", got, wantJwks)
	}
}

// TestZeroClientsCarriesNoProviderKeys is the other half, and the reason
// the keys are inside the has-clients branch. With no OidcClient the
// file must stay comment-only: emitting identity_providers.oidc with an
// hmac_secret and no clients gives Authelia a provider it refuses to
// start with, and emitting a secret reference into a file that would
// otherwise carry none re-creates the first-install failure the
// materialize flag was just fixed for.
func TestZeroClientsCarriesNoProviderKeys(t *testing.T) {
	got := string(renderAutheliaClients(nil))
	for _, ref := range []string{autheliaHmacSecretRef, autheliaJwksSecretRef, "jwks:"} {
		if strings.Contains(got, ref) {
			t.Errorf("a deployment with no OIDC clients rendered %q:\n%s", ref, got)
		}
	}
	if configFileNeedsMaterialization(got) {
		t.Error("the zero-client clients file carries a secret reference, so the node will be asked " +
			"to materialize it — which is exactly the first-install failure just fixed")
	}
}

// TestTailnetRegistersTheServedCallbackToo is F79 seen from the
// provider's side. A package registers its callback under the host it
// declared; under the tailnet posture the browser is at
// <tailnet-name>:<port>, an app that builds the callback from the
// request sends that, and Authelia refuses a redirect_uri it was not
// told about. Both forms must be registered — and under a hostname
// posture nothing may change, or every deployment's clients file would
// be rewritten by this commit.
func TestTailnetRegistersTheServedCallbackToo(t *testing.T) {
	client := oidcClient("open-webui", func(s *resource.OidcClientSpec) {
		s.RedirectUris = []string{"https://chat.example.com/oauth/oidc/callback", "app.example:///callback"}
	})
	route := tailnetRoute("chat", "chat.example.com")

	under := string(renderAutheliaClients([]resource.Resource{
		tailnetIngress("wildstone.tailnet.ts.net"), route, client,
	}))
	for _, want := range []string{
		"- 'https://chat.example.com/oauth/oidc/callback'",
		"- 'https://wildstone.tailnet.ts.net:16443/oauth/oidc/callback'", // chat is pinned to 16443
		"- 'app.example:///callback'",
	} {
		if !strings.Contains(under, want) {
			t.Errorf("tailnet rendering lacks %s:\n%s", want, under)
		}
	}
	if n := strings.Count(under, "app.example:///callback"); n != 1 {
		t.Errorf("app-scheme callback rendered %d times, want 1", n)
	}

	// No Route declares the host: nothing to say about where it is
	// served, so nothing is added.
	orphan := string(renderAutheliaClients([]resource.Resource{
		tailnetIngress("wildstone.tailnet.ts.net"), client,
	}))
	if strings.Contains(orphan, "tailnet.ts.net") {
		t.Errorf("a callback on a host no Route serves grew a tailnet twin:\n%s", orphan)
	}

	// Hostname posture: byte-for-byte what it was.
	plain := string(renderAutheliaClients([]resource.Resource{route, client}))
	if strings.Contains(plain, "tailnet") || strings.Count(plain, "callback'") != 2 {
		t.Errorf("hostname posture changed the rendering:\n%s", plain)
	}
}

// A public client (the Console's, ADR-W033 A4.1) has no secret to
// render and authenticates at the token endpoint with none — the
// client_secret line a confidential client carries must not appear, not
// even as a reference.
func TestAPublicClientRendersWithNoSecret(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("wildstone-console", func(s *resource.OidcClientSpec) {
			s.Public, s.RequirePkce, s.SecretRef = true, true, ""
		}),
	}))
	block := clientBlock(t, got, "wildstone-console")
	for _, want := range []string{"public: true", "token_endpoint_auth_method: 'none'", "require_pkce: true", "pkce_challenge_method: 'S256'"} {
		if !strings.Contains(block, want) {
			t.Errorf("public client lacks %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "client_secret") {
		t.Errorf("public client rendered a secret:\n%s", block)
	}
}

func TestExternalEnrollmentClientRendersAsConsentOnlyPublicPKCE(t *testing.T) {
	got := string(renderAutheliaClients([]resource.Resource{
		oidcClient("wildstone-external-enrollment", func(s *resource.OidcClientSpec) {
			s.Public = true
			s.RequirePkce = true
			s.SecretRef = ""
			s.ConsentMode = "explicit"
			s.Scopes = []string{"openid", "profile"}
			s.IdTokenClaims = []string{"preferred_username"}
		}),
	}))
	block := clientBlock(t, got, "wildstone-external-enrollment")
	for _, want := range []string{
		"public: true", "token_endpoint_auth_method: 'none'", "consent_mode: 'explicit'",
		"require_pkce: true", "pkce_challenge_method: 'S256'", "- 'openid'", "- 'profile'",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("enrollment client lacks %q:\n%s", want, block)
		}
	}
	for _, forbidden := range []string{"client_secret", "wildstone.agent", "agent"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("enrollment client contains forbidden %q:\n%s", forbidden, block)
		}
	}
}
