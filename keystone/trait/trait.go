// Package trait is the vocabulary of what packages advertise to and
// depend on one another for — and, for each of those, the exact values a
// provider hands its consumers through `.Resolved`.
//
// It exists because that vocabulary used to have no written form. A
// package could say `requires: [{trait: identiy, mode: oidc}]` and lint
// would pass it: `requires` was a free string matched against other
// packages' equally free `provides`, so a typo was not a malformed
// package, merely one that nothing satisfied — and the symptom arrived at
// install time, as "unsatisfied requirement identiy:oidc", naming the
// typo but not saying it was one. On the `provides` side it was worse:
// a package advertising a trait nobody spells the same way is silently
// useful to nobody, and there is no failure at all.
//
// The second half is the one that shaped this package. `.Resolved` — the
// map a template renders over — was built by hardcoded Go: keystone/
// capability looked up "identity"/"oidc" by name and computed IssuerURL,
// SessionDomain and DashboardURL from the provider's params. That works
// exactly once. A second provider trait wanting to hand a consumer
// anything (a database its DSN, a message broker its address) is not a
// package someone writes; it is a patch to the engine. "Add a service to
// the catalog" was data, and "integrate with a service in the catalog"
// was a code change, which is the wrong way round for the half we want
// third parties doing.
//
// So: a trait declares which `.Resolved` keys it carries, and a PROVIDER
// package declares, in its own YAML, how to fill them (pkgspec's
// Provide.Exports). The engine stops knowing what "identity" is.
//
// # This vocabulary is closed, and that is deliberate
//
// Adding a trait is a reviewed change to this one file — the same posture
// as policy.Classifications, pkgspec's publishShapes, and the registry
// index. A vocabulary that grows by guessing cannot tell a rule from an
// accident: nobody can say whether `trait: storag` was refused because it
// is wrong or accepted because the check is weak.
//
// The closed half is small on purpose. What a household actually adds —
// a service, its containers, its routes, its events, the values it hands
// to others — is all data in a package.yaml. Only the NAMES those
// packages meet each other under live here, and a new one is four lines
// plus a description.
//
// This package is a leaf: it imports nothing from keystone, so pkgspec
// (schema), pkgtool (lint) and capability (expansion) can all read the
// same vocabulary without any of them importing another.
package trait

import "sort"

// Export is one `.Resolved` key carried by a trait mode.
//
// Description is not decoration: it is what a package author reads when
// deciding whether a key is the one they want, and `wild pkg traits`
// prints it. A key nobody can describe is a key nobody should depend on.
type Export struct {
	Key         string
	Description string
}

// Mode is one variant of a trait — the concrete protocol or posture a
// provider implements. `identity` is not enough to integrate against;
// `identity:oidc` is.
type Mode struct {
	Name        string
	Description string
	// Exports are the `.Resolved` keys a provider of this mode fills.
	// Empty means the mode is a statement of fact ("this deployment has
	// an LDAP directory") with nothing to hand over.
	Exports []Export
}

// Class is a non-functional qualifier on a trait — how much, how fast,
// how durable — as opposed to Mode, which is what protocol.
//
// Kept separate because they compose differently: a requirement names at
// most one mode but may care about a class independently, and
// keystone/capability matches modes while classes are declarative only.
type Class struct {
	Name        string
	Description string
}

// Trait is one entry in the vocabulary.
type Trait struct {
	Name        string
	Description string
	// Structural means the SYSTEM satisfies this trait, not a package
	// (ADR-W010 A1): the engine never looks for a provider, because the
	// mechanism is built in. Every structural trait must name that
	// mechanism in its Description — a structural trait whose mechanism
	// nobody can point at is a requirement that silently never fails.
	Structural bool
	Modes      []Mode
	Classes    []Class
}

// Intrinsic are the `.Resolved` keys every package gets regardless of
// what it requires, because they are derived from the package itself or
// from the node's own state rather than from any provider.
//
// Listed here, next to the trait-carried keys, so there is ONE answer to
// "what may a template reference" rather than one in this file and one in
// a comment on buildResolved.
var Intrinsic = []Export{
	{
		Key: "OidcSecretRef",
		Description: "\"secret/<capability>-oidc\" — the client secret an OidcClient template " +
			"should name in secretRef. The engine synthesises the Secret to match, so a " +
			"package that uses this reference does not also declare it.",
	},
	{
		Key: "PublisherSecretRef",
		Description: "\"secret/<capability>-publisher\" — the bearer token a package that " +
			"declares `publishes` presents to the household intake (ADR-W024 A10). Put it " +
			"in the shim's env, or in the config it renders for an external consumer.",
	},
	{
		Key: "TailnetHost",
		Description: "The node's MagicDNS name under the tailnet ingress posture, and the " +
			"empty string otherwise. Always defined, so a template may test it without " +
			"tripping missingkey=error.",
	},
}

// ExportFuncs are the template functions an export value may call,
// alongside Go's own built-ins (print, printf, if/with, and the rest).
//
// A closed list for the usual reason, and a short one for a specific
// one: an export is evaluated at expansion time, before any Route exists,
// so the only questions it can ask are the ones keystone/ingressorigin
// can answer from the node's declared posture. Names here; the
// implementations live in keystone/capability, which is the only package
// that has a posture to bind them to. pkgspec parses export templates
// against these names alone, so a typo is a schema error the author sees
// at lint, not a render failure at install.
var ExportFuncs = []string{
	// origin <host> — the browser-facing origin the given declared host
	// is actually SERVED at, e.g. "https://auth.example.com", or
	// "https://node.tailnet.ts.net:7443" under the tailnet posture
	// (ADR-W019 A10).
	"origin",
	// sessionDomain <domain> — the cookie domain a session must be
	// scoped to for that domain to work under the current posture.
	"sessionDomain",
}

// registry is the vocabulary itself. Ordered, so diagnostics and
// `wild pkg traits` list traits the same way every time.
//
// TO ADD A TRAIT: append an entry, describe it in words a package author
// who has never read this file can act on, and — if it carries exports —
// say what each key means precisely enough that a provider knows what to
// put there and a consumer knows what it will get. trait_test.go will
// tell you what else the entry owes: a description on everything, no
// duplicate names, and an export key no other mode already carries.
var registry = []Trait{
	{
		Name: "identity",
		Description: "Who someone is, and the sign-in that establishes it. A deployment " +
			"has at most one identity provider; every service that wants single sign-on " +
			"requires this rather than shipping its own login.",
		Modes: []Mode{
			{
				Name: "oidc",
				Description: "An OpenID Connect provider a service can register a client " +
					"with. The consumer renders an OidcClient resource; Keystone merges it " +
					"into the provider's client list.",
				Exports: []Export{
					{
						Key: "IssuerURL",
						Description: "The provider's browser-facing portal origin. This string " +
							"is compared byte for byte against what the browser saw, so it must " +
							"be where the portal is SERVED, not where its Route was declared.",
					},
					{
						Key: "SessionDomain",
						Description: "The cookie domain single sign-on is scoped to. Must " +
							"contain IssuerURL's host, and must never be a public suffix or a " +
							"parent of one — browsers refuse such a cookie outright.",
					},
					{
						Key: "DashboardURL",
						Description: "Where a sign-in with no particular destination lands: " +
							"the page the household thinks of as home.",
					},
				},
			},
			{
				Name: "ldap",
				Description: "A directory a service can bind to and search, for the services " +
					"that authenticate against LDAP rather than redirecting to a portal. " +
					"Carries no exports: a consumer names the directory container and its " +
					"base DN in its own params, because those are deployment facts rather " +
					"than provider-derived ones.",
			},
		},
	},
	{
		Name: "ingress",
		Description: "Reaching a service from outside its container. Structural: satisfied " +
			"by the Route/reverse-proxy machinery (ADR-W004 A1), which every deployment " +
			"has, so the engine never looks for a provider. The ingress PACKAGES still " +
			"advertise it, because which modes a deployment can actually serve is a real " +
			"question the Console asks — it is the engine that does not need an answer.",
		Structural: true,
		Modes: []Mode{
			{
				Name: "public",
				Description: "Reachable from the public internet. A statement about the bind " +
					"address, not about who signed the certificate: a publicly trusted cert " +
					"on a tailnet-only listener is still private.",
			},
			{
				Name:        "private",
				Description: "Reachable only from the household's own network or tailnet.",
			},
		},
	},
	{
		Name: "storage",
		Description: "Somewhere for a service to keep data. Structural: satisfied by the " +
			"Volumes a package declares (ADR-W004 A1). The class is declarative — it says " +
			"what the package expects of the disk it lands on, which an operator siting a " +
			"node can act on and the engine deliberately does not enforce.",
		Structural: true,
		Classes: []Class{
			{
				Name: "bulk",
				Description: "Large, growing, and cheap per gigabyte — a photo library, a " +
					"media collection, a document archive. Not where you want a database.",
			},
		},
	},
}

// All returns the vocabulary in registry order.
func All() []Trait {
	out := make([]Trait, len(registry))
	copy(out, registry)
	return out
}

// Names are the trait names, sorted, for "must be one of ..." messages.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, t := range registry {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Lookup finds a trait by name.
func Lookup(name string) (Trait, bool) {
	for _, t := range registry {
		if t.Name == name {
			return t, true
		}
	}
	return Trait{}, false
}

// IsStructural reports whether the named trait is satisfied by the system
// rather than by a package. An unknown trait is not structural: the
// engine must go looking for a provider and fail honestly when there is
// none, rather than treating a name it does not recognise as free.
func IsStructural(name string) bool {
	t, ok := Lookup(name)
	return ok && t.Structural
}

// LookupMode finds one of the trait's modes by name.
func (t Trait) LookupMode(name string) (Mode, bool) {
	for _, m := range t.Modes {
		if m.Name == name {
			return m, true
		}
	}
	return Mode{}, false
}

// ModeNames are the trait's mode names, sorted.
func (t Trait) ModeNames() []string {
	out := make([]string, 0, len(t.Modes))
	for _, m := range t.Modes {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// HasClass reports whether the trait declares the named class.
func (t Trait) HasClass(name string) bool {
	for _, c := range t.Classes {
		if c.Name == name {
			return true
		}
	}
	return false
}

// ClassNames are the trait's class names, sorted.
func (t Trait) ClassNames() []string {
	out := make([]string, 0, len(t.Classes))
	for _, c := range t.Classes {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// HasExport reports whether the mode carries the named `.Resolved` key.
func (m Mode) HasExport(key string) bool {
	for _, e := range m.Exports {
		if e.Key == key {
			return true
		}
	}
	return false
}

// ExportKeys are the mode's `.Resolved` keys, sorted.
func (m Mode) ExportKeys() []string {
	out := make([]string, 0, len(m.Exports))
	for _, e := range m.Exports {
		out = append(out, e.Key)
	}
	sort.Strings(out)
	return out
}

// ExportKeyOwner names the trait and mode that carry a `.Resolved` key,
// so a diagnostic about an unknown key can say where the right one lives
// instead of only that this one is wrong. Returns ("", "", false) for a
// key no trait carries; intrinsic keys are not trait-carried and are not
// found here.
func ExportKeyOwner(key string) (traitName, modeName string, ok bool) {
	for _, t := range registry {
		for _, m := range t.Modes {
			if m.HasExport(key) {
				return t.Name, m.Name, true
			}
		}
	}
	return "", "", false
}

// IsIntrinsic reports whether a `.Resolved` key is one every package gets
// regardless of what it requires.
func IsIntrinsic(key string) bool {
	for _, e := range Intrinsic {
		if e.Key == key {
			return true
		}
	}
	return false
}

// IntrinsicKeys are the intrinsic `.Resolved` keys, sorted.
func IntrinsicKeys() []string {
	out := make([]string, 0, len(Intrinsic))
	for _, e := range Intrinsic {
		out = append(out, e.Key)
	}
	sort.Strings(out)
	return out
}
