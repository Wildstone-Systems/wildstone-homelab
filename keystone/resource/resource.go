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
	"fmt"
	"regexp"
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

// Metadata is the ownership/placement envelope shared by every resource.
type Metadata struct {
	// Name is unique per kind.
	Name string `yaml:"name"`
	// Owner is the ownership chain: "capability/<name>" or "system".
	// Nothing is orphaned (ADR-W004 A1).
	Owner string `yaml:"owner"`
	// Node is the Wardstone node id this resource is applied to. Empty for
	// kinds that are not node-scoped (e.g. DnsRecord, applied via an
	// external-resource provider rather than Wardstone).
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
	// Ports are "host:container" pairs.
	Ports   []string `yaml:"ports,omitempty"`
	Mounts  []Mount  `yaml:"mounts,omitempty"`
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
	// Auth is one of routeAuthValues: none, authelia_oidc, app_oidc, portal.
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
//   - "portal":        the identity provider's OWN login surface — public
//     by necessity (a login page cannot sit behind itself).
var routeAuthValues = map[string]bool{
	"none":          true,
	"authelia_oidc": true,
	"app_oidc":      true,
	"portal":        true,
}

// oidcAuthPolicies is the closed set of supported OidcClientSpec.AuthPolicy
// values (Authelia authorization_policy).
var oidcAuthPolicies = map[string]bool{
	"one_factor": true,
	"two_factor": true,
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
	// AuthPolicy is the Authelia authorization_policy, e.g. "two_factor" or
	// "one_factor".
	AuthPolicy string `yaml:"authPolicy"`
	// SecretRef is a *reference* to the client secret, e.g.
	// "secret/immich-oidc" — never a literal secret value (ADR-W005 A2).
	// Only Wardstone resolves it to a real value at materialization, out of
	// scope here; the rendered fragment carries a placeholder derived from
	// this reference, never the secret itself.
	SecretRef string `yaml:"secretRef"`
}

// LdapGroupSpec is the spec for kind LdapGroup (ADR-W005 A3). Modeled as
// declared + best-effort: it expands and validates, and its reconcile
// apply is a documented log-and-record no-op against real LLDAP — real
// LLDAP provisioning is a later provider (keystone/reconcile).
type LdapGroupSpec struct {
	Name    string   `yaml:"name"`
	Members []string `yaml:"members,omitempty"`
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
}

// registry is the closed Go map from Kind to its factory/validator.
// Adding a kind is a code change (ADR-W004 A1).
var registry = map[Kind]kindEntry{
	KindContainer: {
		newSpec:  func() any { return &ContainerSpec{} },
		validate: validateContainer,
	},
	KindVolume: {
		newSpec:  func() any { return &VolumeSpec{} },
		validate: validateVolume,
	},
	KindRoute: {
		newSpec:  func() any { return &RouteSpec{} },
		validate: validateRoute,
	},
	KindDnsRecord: {
		newSpec:  func() any { return &DnsRecordSpec{} },
		validate: validateDnsRecord,
	},
	KindOidcClient: {
		newSpec:  func() any { return &OidcClientSpec{} },
		validate: validateOidcClient,
	},
	KindLdapGroup: {
		newSpec:  func() any { return &LdapGroupSpec{} },
		validate: validateLdapGroup,
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

// validatePortMapping checks a single ContainerSpec.Ports entry against the
// "host:container" numeric-port format the reconciler expects.
func validatePortMapping(r *Resource, p string) error {
	parts := strings.Split(p, ":")
	if len(parts) != 2 {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.ports", Msg: fmt.Sprintf("%q must be \"host:container\"", p)}
	}
	for _, part := range parts {
		port, err := strconv.Atoi(part)
		if err != nil || port < 1 || port > 65535 {
			return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.ports", Msg: fmt.Sprintf("%q must be \"host:container\" with numeric ports in 1-65535", p)}
		}
	}
	return nil
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
	if r.Route.Classification == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.classification", Msg: "must not be empty (mandatory policy input)"}
	}
	if r.Route.Host == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.host", Msg: "must not be empty"}
	}
	if !routeAuthValues[r.Route.Auth] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.auth", Msg: "must be one of none, authelia_oidc, app_oidc, portal"}
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
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.authPolicy", Msg: "must be one of one_factor, two_factor"}
	}
	if !secretRefRegexp.MatchString(r.OidcClient.SecretRef) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.secretRef", Msg: "must be a reference of the form secret/<name> (ADR-W005 A2), never a literal secret value"}
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
	return nil
}
