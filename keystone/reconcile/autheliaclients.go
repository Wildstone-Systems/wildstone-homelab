package reconcile

// The merged Authelia clients file.
//
// Every other renderer in this package writes one file per resource,
// which works because Caddy's `import routes/*.caddy` unions them.
// Authelia does not union: given several config files that each define
// identity_providers.oidc.clients, the last one REPLACES the list rather
// than appending to it, so N per-client files leave exactly one client
// registered and no error anywhere. Two OIDC apps installed, one able to
// log in, nothing in any log saying why.
//
// So the clients Authelia actually reads live in ONE file, rendered from
// every OidcClient desired on the node. The per-client fragments in
// oidcclient.go stay, and are not Authelia inputs: they are how observe
// and diff keep a per-resource identity (present/absent/drifted for THIS
// client), which a single merged file cannot express — a whole-file
// comparison would report every client drifted when one changed. One file
// per resource for Wildstone's bookkeeping, one merged file for the
// consumer that cannot read the former.

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// autheliaClientsPath is the merged file, the one Authelia is pointed at.
func autheliaClientsPath(managedPrefix string) string {
	return path.Join(managedPrefix, "authelia", "clients.yml")
}

// claimsPolicyName and authPolicyName derive the per-client policy names.
// Deriving rather than letting a package choose keeps two clients from
// colliding on one name and quietly inheriting each other's rules, and
// client_id is already unique by construction.
func claimsPolicyName(clientID string) string { return clientID + "_claims" }
func authPolicyName(clientID string) string   { return clientID + "_groups" }

// renderAutheliaClients renders the merged identity_providers.oidc block
// for every OidcClient in desired, on this node.
//
// Deterministic for the same reason every other renderer here is: the
// content is byte-compared against the live file to decide whether it
// drifted. Clients are sorted by client_id, and every list rendered from
// a spec is already an ordered slice.
//
// client_secret is NEVER a real secret. Packages carry only a reference
// (ADR-W005 A2) and this emits that reference, marked for digest
// materialization; only Wardstone resolves references, on the node,
// out of scope here.
func renderAutheliaClients(desired []resource.Resource) []byte {
	// Each client's policy is the one its posture enforces (see
	// tailnetPolicy): the access rules and the OIDC clients must agree,
	// or an app signs somebody in with a password that the page in front
	// of it would not have. A copy, so desired state is not edited.
	posture := ingressorigin.Of(desired)
	clients := make([]*resource.OidcClientSpec, 0, len(desired))
	for _, r := range desired {
		if r.Kind == resource.KindOidcClient && r.OidcClient != nil {
			c := *r.OidcClient
			c.AuthPolicy = tailnetPolicy(c.AuthPolicy, posture)
			clients = append(clients, &c)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].ClientId < clients[j].ClientId })

	var b strings.Builder
	b.WriteString("# managed by wildstone-keystone — do not edit\n")
	b.WriteString("identity_providers:\n")
	b.WriteString("  oidc:\n")

	// Authelia refuses to start with an empty clients list, so a node with
	// no OidcClient gets a file with no clients KEY at all rather than one
	// with an empty list. An identity provider that will not boot is worse
	// than one serving nothing.
	if len(clients) == 0 {
		b.WriteString("    # no OIDC clients are desired on this node\n")
		return []byte(b.String())
	}

	renderProviderKeys(&b)
	renderClaimsPolicies(&b, clients)
	renderAuthorizationPolicies(&b, clients)

	served := servedOriginFor(desired)
	b.WriteString("    clients:\n")
	for _, c := range clients {
		renderClient(&b, c, served)
	}
	return []byte(b.String())
}

// servedOriginFor answers, for a host some Route in desired declares,
// the browser-facing host:port it is actually SERVED at — or "" when
// that is the declared host itself, which is every posture but the
// tailnet's (ADR-W019 A10).
//
// A package registers its OIDC callback under the host it declared, and
// under the tailnet posture the browser is never there: it is at
// <tailnet-name>:<port>. An app that builds the callback from the
// request it received sends that served form, and Authelia rejects a
// redirect_uri it was not told about — the F79 failure, seen from the
// provider's side. The renderer therefore registers BOTH forms: the
// declared one for an app that builds the callback from its configured
// base URL, the served one for an app that builds it from the request.
// Nothing is rewritten, so every hostname posture renders exactly what
// it did before.
func servedOriginFor(desired []resource.Resource) func(host string) string {
	posture := ingressorigin.Of(desired)
	declared := map[string]bool{}
	for _, r := range desired {
		if r.Kind == resource.KindRoute && r.Route != nil {
			declared[r.Route.Host] = true
		}
	}
	return func(host string) string {
		if !declared[host] {
			return ""
		}
		if addr := posture.SiteAddress(host); addr != host {
			return addr
		}
		return ""
	}
}

// redirectURIs is the registered list for one client: what the package
// declared, plus the served-origin twin of each https URI whose host a
// Route in this deployment serves somewhere else. Order is preserved
// and duplicates dropped, so the file stays byte-stable across ticks.
func redirectURIs(c *resource.OidcClientSpec, served func(string) string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, raw := range c.RedirectUris {
		add(raw)
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" {
			continue
		}
		if at := served(u.Host); at != "" {
			twin := *u
			twin.Host = at
			add(twin.String())
		}
	}
	return out
}

// renderClaimsPolicies emits one claims policy per client that wants
// claims in the ID token.
//
// Requesting a scope is not enough: Authelia delivers a scope's claims at
// /userinfo, and an app that reads groups from the ID token sees nothing
// at all. The scope is still required — the policy adds nothing the
// client was not granted.
func renderClaimsPolicies(b *strings.Builder, clients []*resource.OidcClientSpec) {
	var withClaims []*resource.OidcClientSpec
	for _, c := range clients {
		if len(c.IdTokenClaims) > 0 {
			withClaims = append(withClaims, c)
		}
	}
	if len(withClaims) == 0 {
		return
	}
	b.WriteString("    claims_policies:\n")
	for _, c := range withClaims {
		fmt.Fprintf(b, "      %s:\n", claimsPolicyName(c.ClientId))
		b.WriteString("        id_token:\n")
		for _, claim := range c.IdTokenClaims {
			fmt.Fprintf(b, "          - '%s'\n", claim)
		}
	}
}

// renderAuthorizationPolicies emits one policy per group-gated client:
// deny by default, admit only the named groups.
//
// This is the OIDC counterpart of Jellyfin's LdapSearchFilter, and for an
// app that cannot enforce group membership itself it is the only gate
// there is — Paperless discards the groups claim entirely, so a user it
// admits has whatever permissions Paperless gives a new account, which is
// none, which looks like a broken install rather than a refusal. Denying
// the token is the difference between "you may not" and "you may, and
// nothing works".
func renderAuthorizationPolicies(b *strings.Builder, clients []*resource.OidcClientSpec) {
	var gated []*resource.OidcClientSpec
	for _, c := range clients {
		if len(c.RequireGroups) > 0 {
			gated = append(gated, c)
		}
	}
	if len(gated) == 0 {
		return
	}
	b.WriteString("    authorization_policies:\n")
	for _, c := range gated {
		fmt.Fprintf(b, "      %s:\n", authPolicyName(c.ClientId))
		b.WriteString("        default_policy: 'deny'\n")
		b.WriteString("        rules:\n")
		fmt.Fprintf(b, "          - policy: '%s'\n", effectiveAuthPolicy(c))
		b.WriteString("            subject:\n")
		for _, g := range c.RequireGroups {
			fmt.Fprintf(b, "              - 'group:%s'\n", g)
		}
	}
}

// effectiveAuthPolicy is the policy a group-gated client's admit-rule
// carries. "deny" as the spec's AuthPolicy alongside RequireGroups would
// mean "admit these groups, then deny them", so the rule falls back to
// one_factor and the default_policy keeps everyone else out.
func effectiveAuthPolicy(c *resource.OidcClientSpec) string {
	if c.AuthPolicy == "deny" {
		return "one_factor"
	}
	return c.AuthPolicy
}

func renderClient(b *strings.Builder, c *resource.OidcClientSpec, served func(string) string) {
	fmt.Fprintf(b, "      - client_id: '%s'\n", c.ClientId)
	fmt.Fprintf(b, "        client_name: '%s'\n", c.ClientName)
	// pbkdf2: asks the node to substitute the DIGEST of this secret
	// (protocol minor 9). Keystone holds no secret values, so what is
	// rendered here is a reference; the agent resolves it when it writes
	// the file, and the result never comes back.
	//
	// Authelia stores the digest and verifies the client's presented
	// value against it, while the app must send that same value in the
	// clear — one secret, two representations, derived at the one place
	// that holds the secret rather than stored twice.
	fmt.Fprintf(b, "        client_secret: 'pbkdf2:%s'\n", c.SecretRef)
	b.WriteString("        public: false\n")

	// A group-gated client points at its own named policy; an ungated one
	// carries the bare policy value.
	if len(c.RequireGroups) > 0 {
		fmt.Fprintf(b, "        authorization_policy: '%s'\n", authPolicyName(c.ClientId))
	} else {
		fmt.Fprintf(b, "        authorization_policy: '%s'\n", c.AuthPolicy)
	}
	if len(c.IdTokenClaims) > 0 {
		fmt.Fprintf(b, "        claims_policy: '%s'\n", claimsPolicyName(c.ClientId))
	}

	// Emitted even when it matches Authelia's own default. The default is
	// client_secret_basic, and a client that POSTs its credentials instead
	// fails with invalid_client naming the method it used — which reads as
	// the client being wrong rather than unregistered. Writing it makes
	// the file say what the app does.
	fmt.Fprintf(b, "        token_endpoint_auth_method: '%s'\n", effectiveTokenAuthMethod(c))
	fmt.Fprintf(b, "        consent_mode: '%s'\n", effectiveConsentMode(c))
	fmt.Fprintf(b, "        userinfo_signed_response_alg: '%s'\n", effectiveUserinfoAlg(c))
	if c.RequirePkce {
		b.WriteString("        require_pkce: true\n")
		b.WriteString("        pkce_challenge_method: 'S256'\n")
	}

	b.WriteString("        redirect_uris:\n")
	for _, u := range redirectURIs(c, served) {
		fmt.Fprintf(b, "          - '%s'\n", u)
	}
	if len(c.Scopes) > 0 {
		b.WriteString("        scopes:\n")
		for _, s := range c.Scopes {
			fmt.Fprintf(b, "          - '%s'\n", s)
		}
	}
}

// The three per-client defaults, in one place so the renderer and any
// future validation cannot disagree about what empty means.
func effectiveTokenAuthMethod(c *resource.OidcClientSpec) string {
	if c.TokenAuthMethod == "" {
		return "client_secret_basic"
	}
	return c.TokenAuthMethod
}

func effectiveConsentMode(c *resource.OidcClientSpec) string {
	if c.ConsentMode == "" {
		return "explicit"
	}
	return c.ConsentMode
}

func effectiveUserinfoAlg(c *resource.OidcClientSpec) string {
	if c.UserinfoSigningAlg == "" {
		return "none"
	}
	return c.UserinfoSigningAlg
}

// autheliaHmacSecretRef and autheliaJwksSecretRef are the secrets the
// OIDC provider itself needs, as distinct from any client's. The
// identity package declares both as Secret resources; these constants
// are the other half of that contract, and the names must agree.
const (
	autheliaHmacSecretRef = "secret/authelia-oidc-hmac-secret"
	autheliaJwksSecretRef = "secret/authelia-oidc-jwks"
)

// renderProviderKeys emits the two things an OIDC provider cannot serve
// without, and which nothing rendered before: the HMAC secret it signs
// JWTs with, and the key it signs ID tokens with.
//
// Both are references the node resolves at write time, so neither value
// exists in Keystone or in a Statefile.
//
// The jwks entry is deliberately minimal. key_id, use and algorithm are
// all optional and auto-derived by Authelia — its own documentation
// calls key_id "generally discouraged unless there is a collision" —
// and a value we set is a value we have to keep correct as the provider
// changes. The key alone is the whole requirement.
//
// The indentation of the reference is load-bearing. It sits inside a
// YAML block scalar, and the PEM that replaces it is many lines; the
// agent's materializer indents every continuation line to the column
// the reference started at, which is what makes a multi-line secret
// survive substitution into structured config at all.
func renderProviderKeys(b *strings.Builder) {
	b.WriteString("    hmac_secret: " + autheliaHmacSecretRef + "\n")
	b.WriteString("    jwks:\n")
	b.WriteString("      - key: |\n")
	b.WriteString("          " + autheliaJwksSecretRef + "\n")
}
