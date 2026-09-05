// Package resource defines Keystone's typed machine-layer records (ADR-W004
// A1): the Resource envelope, the four S2 kind-specific specs (Container,
// Volume, Route, DnsRecord), a closed kind registry, YAML (de)serialization,
// and validation.
//
// status (observedGeneration, conditions) is a runtime-DB concept only and
// is never part of Resource or its serialized form — every Statefile commit
// is an intentional spec change (ADR-W004 A1/A2).
package resource

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind is the closed set of resource kinds Keystone understands. Adding a
// kind is a code change (ADR-W004 A1), mirroring the closed-op-surface
// philosophy of ADR-W003.
type Kind string

const (
	KindContainer  Kind = "Container"
	KindVolume     Kind = "Volume"
	KindRoute      Kind = "Route"
	KindDnsRecord  Kind = "DnsRecord"
	KindOidcClient Kind = "OidcClient"
	KindLdapGroup  Kind = "LdapGroup"
	KindLdapUser   Kind = "LdapUser"
	KindIngress    Kind = "Ingress"
	KindBackupPlan Kind = "BackupPlan"
	KindConfigFile Kind = "ConfigFile"
	KindJob        Kind = "Job"
	KindSecret     Kind = "Secret"
	// KindAutomation is a household rule (ADR-W022 A1): Keystone-owned,
	// applied at no node, run by the automation engine.
	KindAutomation Kind = "Automation"
)

// ApiVersion is the only supported apiVersion for S2 resources.
const ApiVersion = "wildstone.dev/v1"

// digestPinRegexp matches a fully digest-pinned image reference. Reused
// verbatim from ADR-W003 A4.2 / the Wardstone agent's invariant enforcement
// so Keystone rejects tag-pinned images before they ever reach the wire.
var digestPinRegexp = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:-]*@sha256:[a-f0-9]{64}$`)

// secretRefRegexp matches a secret *reference* of the form "secret/<name>"
// (ADR-W005 A2: "a package never contains a secret value; it references
// secret/<name>"). OidcClientSpec.SecretRef must match this — a literal
// secret value here is rejected at validation, before it ever reaches the
// Statefile or a rendered Authelia fragment.
var secretRefRegexp = regexp.MustCompile(`^secret/[A-Za-z0-9._-]+$`)

// nameRegexp bounds Metadata.Name to a DNS-label-like charset. A resource
// name is used verbatim as a filesystem path segment by the Statefile
// (<root>/<kind>/<name>.yaml) and by the reconciler's Caddy/Authelia
// fragment writers, so an unconstrained name (e.g. "../../etc/cron.d/x", an
// absolute path, or one with a NUL/slash) would escape the intended
// directory. Names originate from param-interpolated package templates, so
// this is the choke point that keeps a hostile param from becoming a path
// traversal. Lowercase alphanumerics plus '.', '_' and '-'; must start and
// end alphanumeric; length-bounded.
var nameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

// ldapGroupNameRegexp bounds an LLDAP group name. Narrower than
// nameRegexp on purpose: a group name is interpolated into an LDAP filter
// and a distinguished name (cn=<group>,ou=groups,...), where '.' and '-'
// are legal but invite quoting questions nobody should have to answer,
// and where a stray special character produces a filter that silently
// matches nothing — which, since Jellyfin evaluates that filter on every
// login, locks out users who were working.
var ldapGroupNameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9_]{0,61}[a-z0-9])?$`)

var ldapEmailRegexp = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// ldapDisplayNameRegexp is closed for the same reason the others are: a
// display name reaches the directory inside a JSON document a shell
// script builds by string concatenation, so a quote or a backslash in it
// is a broken document at best. Letters, digits, space and the
// punctuation real names actually use.
var ldapDisplayNameRegexp = regexp.MustCompile(`^[\p{L}\p{N} .'-]{1,64}$`)

// Metadata is the ownership/placement envelope shared by every resource.
type Metadata struct {
	// Name is unique per kind.
	Name string `yaml:"name"`
	// Owner is the ownership chain: "capability/<name>" or "system".
	// Nothing is orphaned (ADR-W004 A1).
	Owner string `yaml:"owner"`
	// Node is the Wardstone node id this resource is applied to. Required
	// for node-scoped kinds and rejected for the rest; the registry's
	// nodeScoped flag is the single source of that split, and Validate
	// enforces it in both directions.
	Node string `yaml:"node,omitempty"`
}

// Mount is a single Container bind mount.
type Mount struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"readOnly,omitempty"`
}

// ContainerSpec is the spec for kind Container, applied via Wardstone
// EnsureContainer/StopContainer/RemoveContainer.
type ContainerSpec struct {
	// Image must be digest-pinned (ADR-W003 A4.2 regex).
	Image string            `yaml:"image"`
	Env   map[string]string `yaml:"env,omitempty"`
	// Ports are "[HOST_IP:]host:container" mappings, in Docker's own
	// syntax. Omitting HOST_IP binds every interface — state an address
	// (an IPv6 one in brackets) to keep a listener off the public ones.
	Ports  []string `yaml:"ports,omitempty"`
	Mounts []Mount  `yaml:"mounts,omitempty"`
	// Command is the container argument vector, appended after the
	// image's entrypoint (docker Cmd; ADR-W011 A3, protocol minor 6).
	Command []string `yaml:"command,omitempty"`
	Restart string   `yaml:"restart,omitempty"`
}

// VolumeSpec is the spec for kind Volume, applied via Wardstone EnsureVolume.
type VolumeSpec struct {
	Name string `yaml:"name"`
}

// RouteSpec is the spec for kind Route, applied by rendering a managed Caddy
// config fragment via Wardstone WriteManagedFile.
type RouteSpec struct {
	Host string `yaml:"host"`
	// Upstream is "container/<name>:<port>" or "ip:port".
	Upstream string `yaml:"upstream"`
	// Auth is one of routeAuthValues: none, authelia_oidc, app_oidc, app_ldap, portal.
	Auth string `yaml:"auth"`
	// Classification is a mandatory policy input, e.g. "family_public".
	Classification string `yaml:"classification"`
}

// DnsRecordSpec is the spec for kind DnsRecord, applied via the external-
// resource provider (Cloudflare) rather than Wardstone (ADR-W004 A5).
type DnsRecordSpec struct {
	Zone string `yaml:"zone"`
	Name string `yaml:"name"`
	// Type is one of "A", "CNAME", "TXT".
	Type    string `yaml:"type"`
	Content string `yaml:"content"`
	Proxied bool   `yaml:"proxied,omitempty"`
}

// dnsRecordTypes is the closed set of supported DnsRecord types.
var dnsRecordTypes = map[string]bool{
	"A":     true,
	"CNAME": true,
	"TXT":   true,
}

// routeAuthValues is the closed set of supported RouteSpec.Auth values.
// The distinctions are policy-load-bearing (ADR-W009 public-requires-auth
// flags only "none"):
//   - "none":          truly unauthenticated — forbidden on family_public
//     routes without an explicit exemption (§9.2-2).
//   - "authelia_oidc": the reverse proxy enforces Authelia forward-auth.
//   - "app_oidc":      the app runs its own OIDC login flow against the
//     identity provider (it registers an OidcClient); the proxy passes
//     through, but the surface is still authenticated.
//   - "app_ldap":      the app runs its own LDAP-backed login against the
//     identity directory (e.g. Jellyfin's LDAP-Auth plugin, ADR-W013 A2);
//     the proxy passes through, but the surface is still authenticated.
//   - "portal":        the identity provider's OWN login surface — public
//     by necessity (a login page cannot sit behind itself).
var routeAuthValues = map[string]bool{
	"none":          true,
	"authelia_oidc": true,
	"app_oidc":      true,
	"app_ldap":      true,
	"portal":        true,
}

// oidcAuthPolicies is the closed set of supported OidcClientSpec.AuthPolicy
// values (Authelia authorization_policy).
var oidcAuthPolicies = map[string]bool{
	"one_factor": true,
	"two_factor": true,
	// A client that exists but must admit nobody. Useful while a client is
	// being provisioned, and as the base an operator can build on with
	// RequireGroups.
	"deny": true,
}

// oidcTokenAuthMethods is the closed set of TokenAuthMethod values. Both
// are in use by first-party packages and neither is a sane default for
// the other, so the field is per-client and this set is deliberately just
// the two the OIDC core spec defines for a confidential client with a
// shared secret.
var oidcTokenAuthMethods = map[string]bool{
	"client_secret_basic": true,
	"client_secret_post":  true,
}

// oidcConsentModes is the closed set of ConsentMode values.
var oidcConsentModes = map[string]bool{
	"explicit":       true,
	"implicit":       true,
	"pre-configured": true,
}

// oidcUserinfoSigningAlgs is the closed set of UserinfoSigningAlg values.
var oidcUserinfoSigningAlgs = map[string]bool{
	"none":  true,
	"RS256": true,
}

// oidcClaims is the closed set of claims that may be pulled into the ID
// token. Closed for the usual reason (ADR-W004 A1) and one more: a claim
// named here is one the renderer must know how to emit, and a typo would
// otherwise produce a config Authelia rejects at startup — taking the
// whole identity provider down rather than the one client.
var oidcClaims = map[string]bool{
	"groups":             true,
	"email":              true,
	"email_verified":     true,
	"name":               true,
	"preferred_username": true,
}

// OidcClientSpec is the spec for kind OidcClient (ADR-W005 A3), applied by
// the identity capability's controller: render the client into an Authelia
// identity_providers.oidc.clients fragment and WriteManagedFile it, then a
// batched Authelia reload (keystone/reconcile).
type OidcClientSpec struct {
	// ClientId is the Authelia OIDC client_id. By convention (mirroring
	// VolumeSpec.Name/Metadata.Name) packages set Metadata.Name == ClientId,
	// since the reconciler's fragment path and observe/diff identity are
	// keyed off Metadata.Name, not this field.
	ClientId     string   `yaml:"clientId"`
	ClientName   string   `yaml:"clientName"`
	RedirectUris []string `yaml:"redirectUris"`
	Scopes       []string `yaml:"scopes,omitempty"`
	// AuthPolicy is the Authelia authorization_policy: "one_factor",
	// "two_factor", or "deny".
	//
	// It is not the whole gate. An app that cannot enforce group
	// membership itself — Paperless discards the groups claim outright —
	// has no authorization at all unless the IdP refuses the token, so
	// RequireGroups turns this into a named policy that also demands
	// membership.
	AuthPolicy string `yaml:"authPolicy"`
	// RequireGroups restricts the client to members of these LLDAP groups.
	// Empty means any user the AuthPolicy admits.
	//
	// This is the OIDC counterpart of Jellyfin's LdapSearchFilter, and for
	// several apps it is the ONLY enforceable gate: the renderer emits a
	// per-client authorization policy that denies by default and admits
	// only these group subjects, so a user outside them is refused a token
	// rather than being let in with no permissions.
	RequireGroups []string `yaml:"requireGroups,omitempty"`
	// TokenAuthMethod is the OIDC token_endpoint_auth_method:
	// "client_secret_basic" (the OIDC default) or "client_secret_post".
	// Empty means client_secret_basic.
	//
	// There is no value that is right for every app, which is why this is
	// per-client rather than a renderer constant: Immich sends the secret
	// in the POST body and fails against the default with invalid_client,
	// while Paperless uses the default.
	TokenAuthMethod string `yaml:"tokenAuthMethod,omitempty"`
	// RequirePkce demands PKCE (S256) from this client. Immich uses PKCE;
	// Paperless does not.
	RequirePkce bool `yaml:"requirePkce,omitempty"`
	// ConsentMode is the Authelia consent_mode: "explicit" (the default —
	// the user is shown a consent screen), "implicit", or "pre-configured".
	// Empty means explicit.
	//
	// A client that cannot render a consent screen needs this. Immich's
	// mobile callback is a custom scheme (app.immich:///oauth-callback)
	// with no browser to consent in, so an explicit-consent client is
	// simply unusable from the app.
	ConsentMode string `yaml:"consentMode,omitempty"`
	// IdTokenClaims are the claims delivered IN the ID token rather than
	// only at /userinfo. Empty means Authelia's default, which puts
	// everything at /userinfo.
	//
	// Requesting a scope is not enough: an app that reads groups from the
	// ID token sees nothing unless the claim is named here, which the
	// renderer turns into a claims_policy.
	IdTokenClaims []string `yaml:"idTokenClaims,omitempty"`
	// UserinfoSigningAlg is the userinfo_signed_response_alg: "none" (the
	// default) or "RS256". It must agree with what the app expects —
	// Immich's profileSigningAlgorithm is the other half of this pair, and
	// the two are configured in files the renderer writes separately.
	UserinfoSigningAlg string `yaml:"userinfoSigningAlg,omitempty"`
	// SecretRef is a *reference* to the client secret, e.g.
	// "secret/immich-oidc" — never a literal secret value (ADR-W005 A2).
	// Only Wardstone resolves it to a real value at materialization, out of
	// scope here; the rendered fragment carries a placeholder derived from
	// this reference, never the secret itself.
	SecretRef string `yaml:"secretRef"`
}

// configFileNameRegexp bounds ConfigFileSpec.Filename: ONE path segment,
// no separators and no leading dot. The rendered path is derived from the
// owning capability rather than taken from the package (see
// ConfigFileSpec), so this is what keeps a filename from walking out of
// the subtree that derivation put it in.
var configFileNameRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ConfigFileSpec is the spec for kind ConfigFile: a configuration file a
// package renders for the app it deploys.
//
// It exists because an app's config is the package's business, not the
// control plane's. Authelia needs an LDAP base DN, a storage backend and
// a notifier; Jellyfin's LDAP plugin needs an XML document; Immich reads
// a JSON file. Rendering those from Keystone would put one profile's
// facts — this identity provider, this schema — inside the component
// that is supposed to be profile-agnostic, which is the layering the
// package system exists to prevent (ADR-W005). A package already knows
// what it deploys; this lets it say so.
//
// The PATH IS DERIVED, never supplied:
//
//	<managedPrefix>/<capability>/<filename>
//
// where <capability> comes from Metadata.Owner. A package therefore
// cannot write into another capability's subtree — not by mistake and
// not on purpose — and the derivation matches the per-capability mount
// each container gets, so a file lands exactly where its own app can
// read it and nowhere else.
// secretKinds is the closed set of SecretSpec.Kind values, mapped to the
// wire enum. Closed for the reason the wire enum is: the AGENT has to
// know how to produce the material, so a new kind is a code change on
// both sides, never a parameter a package can invent.
var secretKinds = map[string]bool{
	// Random bytes. What an HMAC key, a session secret or a storage
	// encryption key wants, and the default when a package says nothing.
	"random": true,
	// A PEM-encoded RSA private key. An OIDC provider signs ID tokens
	// with one and cannot make it up at startup.
	"rsa_private_key": true,
}

// SecretSpec is the spec for kind Secret: a secret that must EXIST on
// the node, generated there, of a stated kind.
//
// It carries no value and never will. The whole point of the secret
// store (ADR-W005 A2, ADR-W012) is that material is generated on the
// node and never crosses the wire, so this kind declares that a secret
// is NEEDED and what shape it has — nothing more. Keystone cannot read
// one back, and neither can a Statefile.
//
// Why a resource kind rather than a package-level block: everything else
// a package causes to happen on a node is a resource, and being one
// means a secret inherits the closed registry, validation, plan
// ordering, approval and audit for free. It also keeps "Authelia's jwks
// key is RSA" inside the identity package, where ADR-W005 puts provider
// knowledge, instead of inside Keystone.
//
// Ordering is the subtle part and is NOT handled by kindOrder alone —
// see ensureNodeSecrets in the reconciler. The managed files Wildstone
// writes before any plan step can carry secret references, so the
// secrets they name have to exist before the plan runs at all.
type SecretSpec struct {
	Name string `yaml:"name"`
	// Kind is one of secretKinds. Empty means "random", so a package
	// that just needs a password says nothing.
	Kind string `yaml:"kind,omitempty"`
	// LengthBytes is the random material to generate before encoding.
	// 0 lets the agent choose (32). Ignored for kinds whose size is not
	// a free parameter, such as an RSA key.
	LengthBytes int `yaml:"lengthBytes,omitempty"`
	// Provided marks a secret the node must NOT generate, because only a
	// human or an external system can produce a correct one. The two
	// shipped examples are a Cloudflare tunnel token and Paperless's
	// OIDC provider block: random bytes in either place are not a weaker
	// secret, they are a broken one, and the failure surfaces as the
	// consuming service misbehaving rather than as anything naming the
	// secret.
	//
	// Declaring it is still the point. Kind and LengthBytes describe a
	// secret's SHAPE; this describes where it comes from, and the two
	// are orthogonal. A package that declares what it needs lets the
	// reconciler name every missing one before convergence starts,
	// instead of the operator meeting them one ERROR_CODE_NOT_FOUND at
	// a time, per tick, after approving a plan that could not have
	// worked (F37).
	Provided bool `yaml:"provided,omitempty"`
}

// JobSpec is the spec for kind Job: a container run ONCE to completion,
// rather than kept running.
//
// It exists because provisioning is not expressible as desired state.
// A directory's users and groups, an application's first admin account,
// a setup wizard that must be answered before an API turns on — these
// are things a running system did, not things it is. Every one of them
// is reached by running a short program against an already-running
// service: lldap ships /app/bootstrap.sh, Immich has
// POST /api/auth/admin-sign-up, Jellyfin has its startup wizard.
//
// The alternative was an Exec op, and that is a rejected change by
// policy rather than by taste (ADR-W003: the operation surface is a
// closed enum, and "adding any generic execution primitive is a
// rejected change"). A Job is not an escape hatch: it names a
// digest-pinned image and runs it under exactly the same ownership,
// mount and network rules as any other container. The privileged
// surface does not grow at all — a Job is applied with the
// EnsureContainer the agent already has.
//
// Run is a full ContainerSpec rather than a flattened copy of its
// fields, so a Job cannot drift away from what a Container may declare:
// the digest pin, the mount rules and the port validation are the same
// code, and specHash covers a Job for free — which is what makes a Job
// re-run when its inputs change.
type JobSpec struct {
	Run ContainerSpec `yaml:"run"`
}

type ConfigFileSpec struct {
	// Filename is one path segment: "configuration.yml", "immich.json".
	Filename string `yaml:"filename"`
	// Content is the rendered file. Secret REFERENCES (secret/<name>,
	// pbkdf2:secret/<name>) are resolved on the node at write time; a
	// literal secret value here is refused by the policy engine, the same
	// as anywhere else (ADR-W005 A2).
	Content string `yaml:"content"`
}

// LdapGroupSpec is the spec for kind LdapGroup: a group that must EXIST
// in the directory. Nothing more.
//
// It carries no membership, and that is a product decision rather than a
// gap. Wildstone declares the groups an app needs — media_users,
// photos_users — and people are added to them by hand in LLDAP's own UI.
// The household knows who lives in it; the control plane does not, and a
// system that thinks it does is one that removes your sister from
// photos_users because a YAML file did not mention her.
//
// A Members field existed here and was never provisioned. Keeping it
// would have been worse than removing it: the tool that would have read
// it (lldap's bootstrap.sh) treats a declared member list as
// authoritative when cleanup is on, so the field's only possible future
// was to start deleting the memberships someone added by hand. Declaring
// half of a relationship is how you end up owning all of it.
//
// If an agent ever adds people, it will do so against the same directory
// through the same API, and the record of who is in a group will still
// be the directory's — not a file.
type LdapGroupSpec struct {
	Name string `yaml:"name"`
}

// ingressTLSModes is the closed set of IngressSpec.TLS values.
//
// Closed because the two are not variations on a theme: they are which
// side of the connection holds the certificate, and getting it wrong
// means either a listener nobody can reach or plaintext where a
// household expected TLS.
var ingressTLSModes = map[string]bool{
	// TLS is terminated upstream and arrives as plain http — a
	// Cloudflare tunnel, or a terminator on the LAN. The listener stays
	// http and Route fragments render http:// (ADR-W019 A4).
	"off": true,
	// The proxy terminates with Caddy's own internal CA (ADR-W019 A3).
	// Route fragments render https:// and the site issues its own
	// certificate, which is what makes a hostname no public CA would
	// sign serve real TLS on a LAN.
	"internal": true,
	// The proxy terminates with a publicly trusted certificate that a
	// locally-running tailscaled already holds for this node's MagicDNS
	// name (ADR-W019 A10). Caddy fetches *.ts.net certificates from
	// tailscaled with no configuration at all, so this posture carries no
	// provider and no credential.
	//
	// The reason it is a posture rather than a variant of "public": a
	// tailnet certificate covers ONE name, the node's own, with no
	// wildcards and no subdomains. Every Route therefore shares a
	// hostname and is told apart by port, which is a different addressing
	// model — not a different way of obtaining the same certificate.
	"tailscale": true,
	// The proxy terminates with a PUBLICLY TRUSTED certificate it gets
	// itself, by ACME over a DNS-01 challenge (ADR-W019 A9).
	//
	// The distinction from "internal" is not how the bytes are encrypted
	// — both are real TLS — but who already trusts the issuer. An
	// internal CA means every device that will ever open the page must
	// first install a root certificate by hand, which is a per-device
	// manual step a household will not complete and cannot be automated
	// from here. DNS-01 is what makes a public CA reachable for a name
	// that resolves to an address the internet cannot route to: the
	// challenge is answered in DNS, so the certificate authority never
	// has to connect to the listener.
	"public": true,
}

// ingressACMEDNSProviders is the closed set of IngressSpec.ACMEDNS values.
//
// Closed for the same reason the TLS modes are: the value is written
// verbatim into a Caddyfile global option naming a module that must be
// compiled into the running Caddy. An unrecognized provider is not a
// degraded certificate, it is a proxy that fails to start.
var ingressACMEDNSProviders = map[string]bool{
	// Cloudflare, via the caddy-dns/cloudflare module. Reads its API
	// token from the CF_API_TOKEN environment variable, which the
	// ingress package supplies from a Provided Secret.
	"cloudflare": true,
}

// IngressSpec is the spec for kind Ingress: how the node's reverse proxy
// is postured, declared by whichever ingress package is installed.
//
// It exists because Keystone's node-level renderers — the base Caddyfile
// and the per-Route fragments — need facts only the ingress PROFILE
// knows, and had no way to learn them. Both were constants: auto_https
// was always off, fragments were always http://, and trusted_proxies was
// a package-level default compiled into the renderer. That is fine while
// one profile ships and wrong the moment two do, which is now:
// caddy-cloudflared sits behind a tunnel that terminates TLS, and
// caddy-tailnet has nothing in front of it and must terminate its own.
//
// A resource rather than a flag or a heuristic, for the reason ADR-W005
// gives and the Secret kind followed: everything else a package causes
// to happen on a node is a resource, and being one means this inherits
// the closed registry, validation, plan ordering, approval and audit
// without rebuilding any of them. ADR-W019 B5 already said the value
// should be "a parameter of the ingress package, not a constant in the
// renderer" — this is that channel.
//
// Deliberately NOT inferred from published ports. "The container
// publishes 443, so it must terminate TLS" is the kind of heuristic this
// repo refuses elsewhere, and it would be load-bearing for whether a
// household's traffic is encrypted.
type IngressSpec struct {
	// TLS is one of ingressTLSModes. Empty means "off", so a profile
	// that terminates nothing says nothing.
	TLS string `yaml:"tls,omitempty"`
	// TrustedProxies is what the listener trusts to have set the
	// X-Forwarded-* headers (ADR-W019 A6). Caddy syntax, e.g.
	// "private_ranges" or a CIDR. Empty trusts nothing, which is the
	// safe default and also the wrong one behind a tunnel — a profile
	// with something in front of it must say so.
	TrustedProxies []string `yaml:"trustedProxies,omitempty"`
	// ACMEDNS names the DNS provider used to answer the DNS-01
	// challenge, one of ingressACMEDNSProviders. Required when TLS is
	// "public" and refused otherwise: a DNS provider is meaningless to a
	// listener that issues its own certificates or terminates none, and
	// silently ignoring it would hide a profile that thinks it is
	// getting a public certificate and is not.
	ACMEDNS string `yaml:"acmeDns,omitempty"`
	// TailnetName is the node's MagicDNS name, e.g.
	// "wildstone.tailnet.ts.net". Required when TLS is "tailscale" and
	// refused otherwise: it is the single name the tailnet certificate
	// covers, and therefore the only host any Route on this node can be
	// served under.
	TailnetName string `yaml:"tailnetName,omitempty"`
}

// LdapUserSpec is the spec for kind LdapUser: a person who must EXIST in
// the directory, and the groups they must be IN.
//
// This carries membership where LdapGroupSpec deliberately does not, and
// the difference is which end of the relationship is being declared. That
// comment's objection is to a group owning its member list: a declared
// list read by a tool with cleanup on starts deleting the memberships
// someone added by hand. Here the person owns the memberships Wildstone
// put them in, the provisioner only ever ADDS, and nothing reads this
// file as an authoritative roster of a group. Adding your sister to
// photos_users in LLDAP's own UI stays untouched, because nothing here
// enumerates who is in photos_users.
//
// It carries NO credential, and never will (ADR-W020 A4). A password
// cannot live in a spec: specs live in the Statefile, and the Statefile
// is replicated, restored and read. The first credential is a single-use
// claim code held as a Secret, which the person exchanges for a password
// they choose.
//
// Deletion is declaration-only, like LdapGroup: removing an LdapUser
// stops Wildstone asserting the person, and does not delete them. A
// control plane that deletes accounts because a YAML file changed is one
// bad plan away from deleting the household.
type LdapUserSpec struct {
	Username    string   `yaml:"username"`
	DisplayName string   `yaml:"displayName,omitempty"`
	Email       string   `yaml:"email,omitempty"`
	Groups      []string `yaml:"groups,omitempty"`
}

// backupSchedules is the closed set of BackupPlanSpec.Schedule values.
// Deliberately coarse (no cron expressions): the owner picks a cadence,
// Keystone picks the moment (ADR-W011 A4 — schedules live in Keystone,
// not cron on the host).
var backupSchedules = map[string]bool{
	"daily":   true,
	"weekly":  true,
	"monthly": true,
}

// BackupRetention is the restic keep-policy for a BackupPlan. Zero values
// mean "no keep flag of that class"; all-zero means no forget/prune pass
// runs at all.
type BackupRetention struct {
	KeepDaily   int `yaml:"keepDaily,omitempty"`
	KeepWeekly  int `yaml:"keepWeekly,omitempty"`
	KeepMonthly int `yaml:"keepMonthly,omitempty"`
}

// BackupPlanSpec is the spec for kind BackupPlan (ADR-W011 A4): a standing
// declaration that the named volumes are backed up into a restic
// repository on the given cadence. Committing one via an approved plan IS
// the owner's standing approval for its runs — the backup engine
// (keystone/backup) executes them as system plans with provenance; the
// reconciler itself treats the kind as declared-only (like LdapGroup).
type BackupPlanSpec struct {
	// Volumes are the source volume names, mounted read-only into the
	// restic run container.
	Volumes []string `yaml:"volumes"`
	// Repository is the volume name holding the restic repository.
	Repository string `yaml:"repository"`
	// PasswordSecret is a reference of the form secret/<name> to the
	// repository password (ADR-W005 A2) — never a literal value. Only
	// Wardstone materializes it (E8).
	PasswordSecret string `yaml:"passwordSecret"`
	// Schedule is one of backupSchedules: daily, weekly, monthly.
	Schedule  string          `yaml:"schedule"`
	Retention BackupRetention `yaml:"retention,omitempty"`
}

// Resource is the typed machine-layer record the Reconciler drives
// (ADR-W004 A1). Exactly one of the Spec fields is populated, matching
// Metadata's Kind (enforced by Validate and by the (de)serialization
// helpers in yaml.go).
//
// status is deliberately absent: it lives only in the runtime DB and must
// never be written to the Statefile.
type Resource struct {
	ApiVersion string   `yaml:"apiVersion"`
	Kind       Kind     `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`

	Container  *ContainerSpec  `yaml:"-"`
	Volume     *VolumeSpec     `yaml:"-"`
	Route      *RouteSpec      `yaml:"-"`
	DnsRecord  *DnsRecordSpec  `yaml:"-"`
	OidcClient *OidcClientSpec `yaml:"-"`
	LdapGroup  *LdapGroupSpec  `yaml:"-"`
	LdapUser   *LdapUserSpec   `yaml:"-"`
	Ingress    *IngressSpec    `yaml:"-"`
	BackupPlan *BackupPlanSpec `yaml:"-"`
	ConfigFile *ConfigFileSpec `yaml:"-"`
	Job        *JobSpec        `yaml:"-"`
	Secret     *SecretSpec     `yaml:"-"`
	Automation *AutomationSpec `yaml:"-"`
}

// ValidationError is a typed validation failure, identifying the resource
// and the field that failed.
type ValidationError struct {
	Kind  Kind
	Name  string
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	who := string(e.Kind)
	if e.Name != "" {
		who = fmt.Sprintf("%s/%s", e.Kind, e.Name)
	}
	return fmt.Sprintf("resource %s: field %s: %s", who, e.Field, e.Msg)
}

// kindEntry is the closed registry's per-kind descriptor: a factory that
// produces an empty spec pointer suitable for decoding into, and a
// validator that checks the fully-populated Resource.
type kindEntry struct {
	newSpec  func() any
	validate func(r *Resource) error
	// nodeScoped records whether this kind is applied to a Wardstone node
	// (and so MUST carry metadata.node) or through a provider (and so must
	// NOT). The reconcile loop buckets desired state by metadata.node and
	// dispatches each bucket at one node; a node-scoped resource with no
	// node lands in the "" bucket, where there is no agent session to
	// observe or apply it. That used to fail deep inside observe with a
	// transport error naming neither the resource nor the missing field —
	// so the invariant the loop relies on is stated here, once, beside the
	// kind vocabulary, and enforced at validation time instead.
	nodeScoped bool
}

// registry is the closed Go map from Kind to its factory/validator.
// Adding a kind is a code change (ADR-W004 A1).
var registry = map[Kind]kindEntry{
	KindContainer: {
		newSpec:    func() any { return &ContainerSpec{} },
		validate:   validateContainer,
		nodeScoped: true,
	},
	KindVolume: {
		newSpec:    func() any { return &VolumeSpec{} },
		validate:   validateVolume,
		nodeScoped: true,
	},
	KindRoute: {
		newSpec:    func() any { return &RouteSpec{} },
		validate:   validateRoute,
		nodeScoped: true,
	},
	KindDnsRecord: {
		// Applied through a Provider (ADR-W004 A5), not an agent.
		newSpec:  func() any { return &DnsRecordSpec{} },
		validate: validateDnsRecord,
	},
	KindOidcClient: {
		// Renders a managed config fragment on the node running the
		// identity provider, so it is as node-scoped as a Route.
		newSpec:    func() any { return &OidcClientSpec{} },
		validate:   validateOidcClient,
		nodeScoped: true,
	},
	KindLdapGroup: {
		// Declared-only today, but the node it is declared against is
		// the one running the directory, and it is what a future
		// applier will provision against.
		newSpec:    func() any { return &LdapGroupSpec{} },
		validate:   validateLdapGroup,
		nodeScoped: true,
	},
	KindLdapUser: {
		// Same reasoning as LdapGroup: declared-only here, and the node
		// it is declared against is the one running the directory.
		newSpec:    func() any { return &LdapUserSpec{} },
		validate:   validateLdapUser,
		nodeScoped: true,
	},
	KindIngress: {
		// Node-scoped: it describes the proxy running on one node, and
		// the renderers that read it are node-level.
		newSpec:    func() any { return &IngressSpec{} },
		validate:   validateIngress,
		nodeScoped: true,
	},
	KindBackupPlan: {
		newSpec:    func() any { return &BackupPlanSpec{} },
		validate:   validateBackupPlan,
		nodeScoped: true,
	},
	KindConfigFile: {
		newSpec:    func() any { return &ConfigFileSpec{} },
		validate:   validateConfigFile,
		nodeScoped: true,
	},
	KindJob: {
		newSpec:    func() any { return &JobSpec{} },
		validate:   validateJob,
		nodeScoped: true,
	},
	KindSecret: {
		// Generated and stored on one node's secret store, so as
		// node-scoped as anything gets.
		newSpec:    func() any { return &SecretSpec{} },
		validate:   validateSecret,
		nodeScoped: true,
	},
	KindAutomation: {
		// Keystone-owned (ADR-W022 A1): the automation engine reads it
		// from the Statefile; no node, no provider, nothing to apply.
		newSpec:  func() any { return &AutomationSpec{} },
		validate: validateAutomation,
	},
}

// KnownKind reports whether k is in the closed registry.
func KnownKind(k Kind) bool {
	_, ok := registry[k]
	return ok
}

// Validate checks common fields (apiVersion, kind known, name, owner, spec
// exclusivity) plus the kind-specific rules from ADR-W004 A1, returning a
// *ValidationError (or a wrapped list) on failure.
func (r *Resource) Validate() error {
	entry, ok := registry[r.Kind]
	if !ok {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "kind", Msg: "unknown kind"}
	}
	// ApiVersion is optional (in-code construction commonly omits it), but a
	// non-empty value that doesn't match the supported ApiVersion is rejected
	// outright — it signals a resource authored against a different schema
	// generation than this binary understands.
	if r.ApiVersion != "" && r.ApiVersion != ApiVersion {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "apiVersion", Msg: fmt.Sprintf("must be %q if set", ApiVersion)}
	}
	if r.Metadata.Name == "" {
		return &ValidationError{Kind: r.Kind, Field: "metadata.name", Msg: "must not be empty"}
	}
	if !nameRegexp.MatchString(r.Metadata.Name) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.name",
			Msg: "must be a DNS-label-like name (lowercase alphanumerics, '.', '_', '-'; start/end alphanumeric; ≤ 63 chars) — it is used as a filesystem path segment"}
	}
	if r.Metadata.Owner == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.owner", Msg: "must not be empty"}
	}
	if entry.nodeScoped && r.Metadata.Node == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.node",
			Msg: "must not be empty: this kind is applied at a node, and a resource with no node is never dispatched to one"}
	}
	if !entry.nodeScoped && r.Metadata.Node != "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.node",
			Msg: fmt.Sprintf("must be empty: %s is applied through a provider, not a node agent", r.Kind)}
	}
	if err := r.validateSpecExclusivity(); err != nil {
		return err
	}
	if err := entry.validate(r); err != nil {
		return err
	}
	return nil
}

// validateSpecExclusivity enforces the doc-commented invariant that exactly
// one Spec field is populated: no spec pointer other than the one matching
// r.Kind may be non-nil. The matching pointer's own nil-ness is left to the
// per-kind validator, which reports it with a more specific message.
func (r *Resource) validateSpecExclusivity() error {
	populated := map[Kind]bool{
		KindContainer:  r.Container != nil,
		KindVolume:     r.Volume != nil,
		KindRoute:      r.Route != nil,
		KindDnsRecord:  r.DnsRecord != nil,
		KindOidcClient: r.OidcClient != nil,
		KindLdapGroup:  r.LdapGroup != nil,
		KindBackupPlan: r.BackupPlan != nil,
		KindConfigFile: r.ConfigFile != nil,
		KindAutomation: r.Automation != nil,
	}
	for k, isSet := range populated {
		if k == r.Kind {
			continue
		}
		if isSet {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: fmt.Sprintf("kind is %s but %s spec is also populated", r.Kind, k)}
		}
	}
	return nil
}

func validateContainer(r *Resource) error {
	if r.Container == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "container spec missing"}
	}
	if !digestPinRegexp.MatchString(r.Container.Image) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.image", Msg: "must be digest-pinned (repo@sha256:<64 hex>), tags are refused"}
	}
	for _, p := range r.Container.Ports {
		if err := validatePortMapping(r, p); err != nil {
			return err
		}
	}
	for i, m := range r.Container.Mounts {
		if m.Source == "" {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: fmt.Sprintf("spec.mounts[%d].source", i), Msg: "must not be empty"}
		}
		if m.Target == "" {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: fmt.Sprintf("spec.mounts[%d].target", i), Msg: "must not be empty"}
		}
	}
	return nil
}

// validateJob reuses validateContainer wholesale — a Job's Run is a
// ContainerSpec and must obey every rule one does — then adds the two
// things that distinguish a run from a service.
//
// Both additions refuse a spec that would still "work": a job with a
// restart policy really does run, over and over; a job with a published
// port really does bind it. They are refused because each is what a
// Container template looks like when it has been copied into a Job
// without being reread, and the failure they cause is remote from the
// mistake — a restart:always job re-provisions in a loop, and a job
// holding port 80 keeps the ingress container from starting long after
// the job itself has been forgotten.
func validateJob(r *Resource) error {
	if r.Job == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "job spec missing"}
	}
	// Borrow the container rules by validating the embedded run against
	// them. The error's Field is rewritten so it points at spec.run.*,
	// where the author has to look, rather than at spec.* of a Container
	// this resource is not.
	if err := validateContainer(&Resource{Kind: r.Kind, Metadata: r.Metadata, Container: &r.Job.Run}); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			ve.Field = strings.Replace(ve.Field, "spec.", "spec.run.", 1)
		}
		return err
	}
	if r.Job.Run.Restart != "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.run.restart",
			Msg: fmt.Sprintf("must not be set (got %q): a Job runs once to completion, and the reconciler "+
				"decides when to run it again from its exit status — a restart policy would re-run it "+
				"on the node's terms instead, forever", r.Job.Run.Restart)}
	}
	if len(r.Job.Run.Ports) > 0 {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.run.ports",
			Msg: "must be empty: a Job talks to services on the container network and is not talked to, " +
				"and a published port it never releases outlives the run that bound it"}
	}
	return nil
}

func validateSecret(r *Resource) error {
	if r.Secret == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "secret spec missing"}
	}
	// The name is used as a filename in the node's secret store and as
	// the target of a secret/<name> reference, so it is bounded by the
	// same rule metadata.name is.
	if !nameRegexp.MatchString(r.Secret.Name) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.name",
			Msg: "must be a DNS-label-like name — it becomes a filename in the node's secret store " +
				"and the target of a secret/<name> reference"}
	}
	// Kind and LengthBytes describe how to GENERATE a secret, and a
	// provided one is never generated. Accepting both would let a
	// package state a shape nothing enforces, which reads as a
	// guarantee and is not one.
	if r.Secret.Provided && (r.Secret.Kind != "" || r.Secret.LengthBytes != 0) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.provided",
			Msg: "a provided secret is supplied by the operator and never generated, so kind and lengthBytes cannot apply to it"}
	}
	if r.Secret.Kind != "" && !secretKinds[r.Secret.Kind] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.kind",
			Msg: fmt.Sprintf("%q is not a kind any agent knows how to generate (want one of %s, or empty for random)",
				r.Secret.Kind, strings.Join(SecretKindNames(), ", "))}
	}
	if r.Secret.LengthBytes < 0 {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.lengthBytes", Msg: "must not be negative"}
	}
	return nil
}

// SecretKindNames returns the closed vocabulary, sorted. Exported so a
// caller naming these in an error or a UI reads the same set the
// validator enforces rather than keeping its own copy.
func SecretKindNames() []string {
	out := make([]string, 0, len(secretKinds))
	for k := range secretKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validatePortMapping checks a single ContainerSpec.Ports entry. The
// reconciler parses the same string with the same function, so anything
// that validates here renders there.
func validatePortMapping(r *Resource, p string) error {
	if _, _, _, ok := SplitPortMapping(p); !ok {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.ports",
			Msg: fmt.Sprintf("%q must be \"[HOST_IP:]host:container\" with numeric ports in 1-65535 "+
				"and, if present, a literal IP address (IPv6 in brackets: \"[::1]:8443:80\")", p)}
	}
	return nil
}

// SplitPortMapping parses one ContainerSpec.Ports entry into the fields of
// a wardstone PortBinding. hostIP is empty when the mapping names no
// address, which is Docker's "every interface" default.
//
// This is the ONLY parser for the form: validation and rendering share it
// so the set of strings that pass review is exactly the set that reaches
// Docker. They were separate functions with separately-drifting rules
// before, which is how "host:container" came to be enforced at the door
// while the wire protocol had carried host_ip since minor 1.
func SplitPortMapping(s string) (hostIP string, hostPort, containerPort uint32, ok bool) {
	rest := s
	switch {
	// Bracketed IPv6, exactly as Docker writes it: "[fd7a::1]:8443:80".
	// Bare IPv6 is unparseable here and Docker rejects it too — the
	// colons are ambiguous against the port separators.
	case strings.HasPrefix(s, "["):
		end := strings.Index(s, "]:")
		if end < 0 {
			return "", 0, 0, false
		}
		hostIP, rest = s[1:end], s[end+2:]
	case strings.Count(s, ":") == 2:
		i := strings.Index(s, ":")
		hostIP, rest = s[:i], s[i+1:]
	case strings.Count(s, ":") != 1:
		return "", 0, 0, false
	}
	if hostIP != "" && net.ParseIP(hostIP) == nil {
		// A hostname would leave the real bind address to Docker's
		// resolver at create time — a private listener must not depend
		// on what /etc/hosts happens to say.
		return "", 0, 0, false
	}
	h, c, found := strings.Cut(rest, ":")
	if !found {
		return "", 0, 0, false
	}
	hp, err1 := strconv.ParseUint(h, 10, 32)
	cp, err2 := strconv.ParseUint(c, 10, 32)
	if err1 != nil || err2 != nil || hp < 1 || hp > 65535 || cp < 1 || cp > 65535 {
		return "", 0, 0, false
	}
	return hostIP, uint32(hp), uint32(cp), true
}

func validateVolume(r *Resource) error {
	if r.Volume == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "volume spec missing"}
	}
	if r.Volume.Name == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.name", Msg: "must not be empty"}
	}
	return nil
}

func validateRoute(r *Resource) error {
	if r.Route == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "route spec missing"}
	}
	if r.Metadata.Name == ReservedRouteName {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.name",
			Msg: "reserved: the reconciler renders its own placeholder fragment to this name, and a Route sharing it oscillates between the two writers (F35)"}
	}
	if r.Route.Classification == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.classification", Msg: "must not be empty (mandatory policy input)"}
	}
	if r.Route.Host == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.host", Msg: "must not be empty"}
	}
	if !routeAuthValues[r.Route.Auth] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.auth", Msg: "must be one of none, authelia_oidc, app_oidc, app_ldap, portal"}
	}
	return nil
}

func validateDnsRecord(r *Resource) error {
	if r.DnsRecord == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "dns record spec missing"}
	}
	if r.DnsRecord.Zone == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.zone", Msg: "must not be empty"}
	}
	if r.DnsRecord.Name == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.name", Msg: "must not be empty"}
	}
	if !dnsRecordTypes[r.DnsRecord.Type] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.type", Msg: "must be one of A, CNAME, TXT"}
	}
	if r.DnsRecord.Content == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.content", Msg: "must not be empty"}
	}
	return nil
}

func validateOidcClient(r *Resource) error {
	if r.OidcClient == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "oidc client spec missing"}
	}
	if r.OidcClient.ClientId == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.clientId", Msg: "must not be empty"}
	}
	if len(r.OidcClient.RedirectUris) == 0 {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.redirectUris", Msg: "must have at least one redirect URI"}
	}
	if !oidcAuthPolicies[r.OidcClient.AuthPolicy] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.authPolicy", Msg: "must be one of one_factor, two_factor, deny"}
	}
	for i, g := range r.OidcClient.RequireGroups {
		if !ldapGroupNameRegexp.MatchString(g) {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: fmt.Sprintf("spec.requireGroups[%d]", i),
				Msg: "must be an LLDAP group name (lowercase alphanumerics and '_'; start/end alphanumeric; ≤ 63 chars)"}
		}
	}
	if m := r.OidcClient.TokenAuthMethod; m != "" && !oidcTokenAuthMethods[m] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.tokenAuthMethod",
			Msg: "must be one of client_secret_basic, client_secret_post (empty means client_secret_basic)"}
	}
	if m := r.OidcClient.ConsentMode; m != "" && !oidcConsentModes[m] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.consentMode",
			Msg: "must be one of explicit, implicit, pre-configured (empty means explicit)"}
	}
	if a := r.OidcClient.UserinfoSigningAlg; a != "" && !oidcUserinfoSigningAlgs[a] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.userinfoSigningAlg",
			Msg: "must be one of none, RS256 (empty means none)"}
	}
	for i, c := range r.OidcClient.IdTokenClaims {
		if !oidcClaims[c] {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: fmt.Sprintf("spec.idTokenClaims[%d]", i),
				Msg: "must be one of groups, email, email_verified, name, preferred_username"}
		}
	}
	if !secretRefRegexp.MatchString(r.OidcClient.SecretRef) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.secretRef", Msg: "must be a reference of the form secret/<name> (ADR-W005 A2), never a literal secret value"}
	}
	return nil
}

func validateConfigFile(r *Resource) error {
	if r.ConfigFile == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "config file spec missing"}
	}
	if !configFileNameRegexp.MatchString(r.ConfigFile.Filename) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.filename",
			Msg: "must be a single path segment (alphanumeric-led [A-Za-z0-9._-], at most 63 chars) — the directory is derived from the owning capability, not supplied"}
	}
	if r.ConfigFile.Content == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.content",
			Msg: "must not be empty: an empty config file is how a delete is expressed, not how one is declared"}
	}
	// The owner is what the path is derived from, so a ConfigFile owned by
	// "system" has no capability subtree to land in. Every other kind
	// tolerates a system owner; this one cannot.
	if _, ok := strings.CutPrefix(r.Metadata.Owner, "capability/"); !ok {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.owner",
			Msg: "must be capability/<name>: the file's directory is derived from the owning capability, and there is nowhere to put one that has none"}
	}
	return nil
}

func validateBackupPlan(r *Resource) error {
	if r.BackupPlan == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "backup plan spec missing"}
	}
	if len(r.BackupPlan.Volumes) == 0 {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.volumes", Msg: "must name at least one source volume"}
	}
	for i, v := range r.BackupPlan.Volumes {
		if v == "" {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: fmt.Sprintf("spec.volumes[%d]", i), Msg: "must not be empty"}
		}
	}
	if r.BackupPlan.Repository == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.repository", Msg: "must name the repository volume"}
	}
	if !secretRefRegexp.MatchString(r.BackupPlan.PasswordSecret) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.passwordSecret", Msg: "must be a reference of the form secret/<name> (ADR-W005 A2), never a literal secret value"}
	}
	if !backupSchedules[r.BackupPlan.Schedule] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.schedule", Msg: "must be one of daily, weekly, monthly"}
	}
	ret := r.BackupPlan.Retention
	if ret.KeepDaily < 0 || ret.KeepWeekly < 0 || ret.KeepMonthly < 0 {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.retention", Msg: "keep counts must not be negative"}
	}
	return nil
}

// validateLdapUser mirrors validateLdapGroup's strictness for the same
// reason: every field here is interpolated into a GraphQL document and an
// LDAP distinguished name by a POSIX shell script with no JSON encoder.
// A closed charset is what makes that safe.
// postureOrOff names the effective TLS posture for an error message,
// since empty means "off" and quoting "" back at an operator is noise.
func postureOrOff(tls string) string {
	if tls == "" {
		return "off"
	}
	return tls
}

func validateIngress(r *Resource) error {
	if r.Ingress == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "ingress spec missing"}
	}
	if r.Ingress.TLS != "" && !ingressTLSModes[r.Ingress.TLS] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.tls",
			Msg: fmt.Sprintf("%q is not a TLS posture (want \"off\", \"internal\", \"public\" or \"tailscale\", or empty for off)", r.Ingress.TLS)}
	}
	switch {
	case r.Ingress.TLS == "public" && r.Ingress.ACMEDNS == "":
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.acmeDns",
			Msg: "required when tls is \"public\": a public certificate for a privately-routed name can only be obtained over DNS-01"}
	case r.Ingress.TLS != "public" && r.Ingress.ACMEDNS != "":
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.acmeDns",
			Msg: fmt.Sprintf("meaningless when tls is %q: only \"public\" obtains a certificate over ACME", postureOrOff(r.Ingress.TLS))}
	case r.Ingress.TLS == "tailscale" && r.Ingress.TailnetName == "":
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.tailnetName",
			Msg: "required when tls is \"tailscale\": the certificate covers exactly one name, so every Route is served under it"}
	case r.Ingress.TLS != "tailscale" && r.Ingress.TailnetName != "":
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.tailnetName",
			Msg: fmt.Sprintf("meaningless when tls is %q: only \"tailscale\" collapses every Route onto one host", postureOrOff(r.Ingress.TLS))}
	case r.Ingress.ACMEDNS != "" && !ingressACMEDNSProviders[r.Ingress.ACMEDNS]:
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.acmeDns",
			Msg: fmt.Sprintf("%q is not a supported ACME DNS provider (want \"cloudflare\")", r.Ingress.ACMEDNS)}
	}
	for _, p := range r.Ingress.TrustedProxies {
		if strings.ContainsAny(p, " \t\n{}") {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.trustedProxies",
				Msg: "entries are written verbatim into a Caddyfile, so whitespace and braces are refused"}
		}
	}
	return nil
}

func validateLdapUser(r *Resource) error {
	if r.LdapUser == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "ldap user spec missing"}
	}
	if r.LdapUser.Username == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.username", Msg: "must not be empty"}
	}
	if !ldapGroupNameRegexp.MatchString(r.LdapUser.Username) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.username",
			Msg: "must be an LLDAP username (lowercase alphanumerics and '_'; start/end alphanumeric; \u2264 63 chars) \u2014 it is interpolated into an LDAP filter and a distinguished name"}
	}
	for _, g := range r.LdapUser.Groups {
		if !ldapGroupNameRegexp.MatchString(g) {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.groups",
				Msg: "group " + g + " is not a valid LLDAP group name"}
		}
	}
	if d := r.LdapUser.DisplayName; d != "" && !ldapDisplayNameRegexp.MatchString(d) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.displayName",
			Msg: "must be letters, digits, spaces, '.', '-' or apostrophe (\u2264 64 chars) \u2014 it is interpolated into a JSON document built by a shell script"}
	}
	// Required, not optional: LLDAP's CreateUserInput takes email as
	// non-null, so a user without one cannot be created at all. Better to
	// say so here than to have the provisioning job fail per-user on the
	// node, or to synthesise an address the household did not choose.
	if r.LdapUser.Email == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.email",
			Msg: "must not be empty — LLDAP requires an address to create an account"}
	}
	// Deliberately narrow rather than RFC-complete: this string is
	// interpolated into a shell-built JSON document, and the characters
	// a full RFC 5322 address may contain include the ones that would
	// break out of it.
	if e := r.LdapUser.Email; e != "" && !ldapEmailRegexp.MatchString(e) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.email",
			Msg: "must be a simple address (letters, digits, '.', '_', '%', '+', '-', one '@', a dotted domain)"}
	}
	return nil
}

func validateLdapGroup(r *Resource) error {
	if r.LdapGroup == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "ldap group spec missing"}
	}
	if r.LdapGroup.Name == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.name", Msg: "must not be empty"}
	}
	if !ldapGroupNameRegexp.MatchString(r.LdapGroup.Name) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.name",
			Msg: "must be an LLDAP group name (lowercase alphanumerics and '_'; start/end alphanumeric; ≤ 63 chars) — it is interpolated into an LDAP filter and a distinguished name"}
	}
	return nil
}
