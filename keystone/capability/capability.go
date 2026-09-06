// Package capability implements the Capability Engine (ADR-W005 A2): given a
// Package (keystone/pkgspec) and a set of params, it validates the params,
// resolves the package's `requires` traits against a Catalog of enabled
// packages, renders the package's templates over a small explicit context,
// and parses+validates the result into keystone/resource.Resource values.
package capability

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/trait"
)

// MissingParamError reports a required ParamDecl with no value supplied, or
// a supplied param not declared by the package (strict mode).
type MissingParamError struct {
	Package string
	Param   string
	// Unknown is true when Param was supplied but not declared by the
	// package (rather than a required-but-missing param).
	Unknown bool
}

func (e *MissingParamError) Error() string {
	if e.Unknown {
		return fmt.Sprintf("capability: package %s: unknown param %q", e.Package, e.Param)
	}
	return fmt.Sprintf("capability: package %s: missing required param %q", e.Package, e.Param)
}

// UnsatisfiedRequirementError reports a `requires` trait/mode with no
// matching provider in the catalog.
type UnsatisfiedRequirementError struct {
	Package string
	Trait   string
	Mode    string
}

func (e *UnsatisfiedRequirementError) Error() string {
	if e.Mode != "" {
		return fmt.Sprintf("capability: package %s: unsatisfied requirement %s:%s", e.Package, e.Trait, e.Mode)
	}
	return fmt.Sprintf("capability: package %s: unsatisfied requirement %s", e.Package, e.Trait)
}

// TemplateError wraps a failure rendering or parsing one of a package's
// templates, identifying which template (by index) failed.
type TemplateError struct {
	Package string
	Index   int
	Err     error
}

func (e *TemplateError) Error() string {
	return fmt.Sprintf("capability: package %s: template[%d]: %s", e.Package, e.Index, e.Err)
}

func (e *TemplateError) Unwrap() error { return e.Err }

// Catalog is the set of enabled packages' metadata/provides, used to
// resolve `requires` traits. It does not itself hold resolved outputs;
// Expand derives those it needs (e.g. IssuerURL) directly from the
// providing package's params.
type Catalog struct {
	pkgs []*pkgspec.Package
	// current is the node's desired state at expansion time. It carries
	// the Ingress declaration, which is where a deployment's ADDRESSING
	// lives (ADR-W019 A8/A10) — and addressing is not derivable from the
	// catalog, because the catalog holds what a package declares, not what
	// an operator installed it with. A .Resolved value that must match
	// what a browser sees has to come from here.
	current []resource.Resource
}

// WithCurrent returns a Catalog that also knows the node's desired state.
//
// Separate from NewCatalog so every existing caller keeps working and
// gets exactly the behaviour it had: with no current state, addressing
// falls back to the declared host, which is what every posture but
// "tailscale" does anyway.
func (c *Catalog) WithCurrent(desired []resource.Resource) *Catalog {
	if c == nil {
		return nil
	}
	out := *c
	out.current = desired
	return &out
}

// NewCatalog builds a Catalog from a set of enabled packages.
func NewCatalog(pkgs ...*pkgspec.Package) *Catalog {
	return &Catalog{pkgs: pkgs}
}

// Provides reports whether some package in the catalog advertises the given
// (trait, mode) pair via its `provides` list.
func (c *Catalog) Provides(traitName, mode string) bool {
	if c == nil {
		return false
	}
	for _, pkg := range c.pkgs {
		for _, p := range pkg.Provides {
			if p.Trait == traitName && p.Mode == mode {
				return true
			}
		}
	}
	return false
}

// Engine expands packages into resources.
type Engine struct{}

// NewEngine constructs an Engine. Engine currently holds no state; it exists
// as the documented entry point per ADR-W005 A2's Engine.Expand signature.
func NewEngine() *Engine { return &Engine{} }

// Expand validates params, resolves requires against catalog, renders each
// of pkg's templates over a context exposing .Params and .Resolved, parses
// and validates the rendered resource, stamps owner if absent, and returns
// the flattened, validated resource list in template order (ADR-W005 A2).
func (e *Engine) Expand(pkg *pkgspec.Package, params map[string]string, catalog *Catalog) ([]resource.Resource, error) {
	merged, err := mergeParams(pkg, params)
	if err != nil {
		return nil, err
	}

	for _, req := range pkg.Requires {
		if trait.IsStructural(req.Trait) {
			// ingress/storage are system mechanisms, always satisfiable
			// (ADR-W010 A1 making ADR-W005 A2's prose real): ingress by the
			// reverse-proxy/Route machinery, storage by declared Volumes.
			// They contribute nothing to .Resolved. Which traits are
			// structural is keystone/trait's to say, so the engine and the
			// vocabulary lint reads cannot disagree about it.
			continue
		}
		if !catalog.Provides(req.Trait, req.Mode) {
			return nil, &UnsatisfiedRequirementError{Package: pkg.Metadata.Name, Trait: req.Trait, Mode: req.Mode}
		}
	}

	resolved, err := buildResolved(pkg, merged, catalog)
	if err != nil {
		return nil, err
	}
	wantOwner := "capability/" + pkg.Metadata.Capability

	ctx := templateContext{Params: merged, Resolved: resolved}

	out := make([]resource.Resource, 0, len(pkg.Templates))
	for i, tmplStr := range pkg.Templates {
		rendered, err := renderTemplate(pkg.Metadata.Name, i, tmplStr, ctx)
		if err != nil {
			return nil, err
		}

		res, err := resource.Unmarshal(rendered)
		if err != nil {
			return nil, &TemplateError{Package: pkg.Metadata.Name, Index: i, Err: fmt.Errorf("parsing rendered resource: %w", err)}
		}

		if res.Metadata.Owner == "" {
			res.Metadata.Owner = wantOwner
		} else if res.Metadata.Owner != wantOwner {
			return nil, &TemplateError{
				Package: pkg.Metadata.Name,
				Index:   i,
				Err:     fmt.Errorf("resource %s/%s: owner %q does not match expected %q", res.Kind, res.Metadata.Name, res.Metadata.Owner, wantOwner),
			}
		}

		if err := res.Validate(); err != nil {
			return nil, &TemplateError{Package: pkg.Metadata.Name, Index: i, Err: fmt.Errorf("validating rendered resource: %w", err)}
		}

		out = append(out, res)
	}

	// A package that renders an OidcClient references a client secret by
	// the name buildResolved synthesised for it, so the same fact has to
	// produce the SECRET as well as the reference. Left to the package,
	// every consumer would repeat the naming convention and one of them
	// would eventually drift from it; worse, forgetting it is invisible
	// until the node refuses to write the merged clients file.
	//
	// Synthesised rather than declared, therefore, but not hidden: it is
	// an ordinary Secret resource, owned by the requiring capability,
	// planned and approved like any other.
	//
	// A package that declares the Secret ITSELF wins, and this adds
	// nothing. Appending regardless produced two Secret resources with
	// one name, the synthesised one last — so documents-paperless, which
	// declares its client secret `provided: true` because Paperless
	// needs the value in plaintext and Wildstone's secrets are
	// write-only, had that declaration silently replaced by a generated
	// one. The package's own statement about a secret it owns is the
	// authority; synthesis is the convenience for packages that make no
	// statement at all.
	declared := map[string]bool{}
	for _, res := range out {
		if res.Kind == resource.KindSecret && res.Secret != nil {
			declared[res.Secret.Name] = true
		}
	}
	for _, res := range out {
		if res.Kind != resource.KindOidcClient || res.OidcClient == nil {
			continue
		}
		name, ok := strings.CutPrefix(res.OidcClient.SecretRef, "secret/")
		if !ok {
			continue // validation already refuses anything else
		}
		if declared[name] {
			continue
		}
		out = append(out, resource.Resource{
			ApiVersion: resource.ApiVersion,
			Kind:       resource.KindSecret,
			Metadata:   resource.Metadata{Name: name, Owner: wantOwner, Node: res.Metadata.Node},
			Secret:     &resource.SecretSpec{Name: name},
		})
	}

	// A package that publishes household events needs a credential to
	// reach the intake, whatever its shape — a native app, a sidecar, or
	// Home Assistant calling a webhook all present the same bearer token
	// (ADR-W024 A10). The token is minted by Keystone and placed on the
	// node as the provided secret `<capability>-publisher`; the package
	// references it as .Resolved.PublisherSecretRef. Synthesised the same
	// way as the OIDC client secret above, and a package that declares
	// the Secret itself still wins.
	if pkg.Publishes != nil {
		name := PublisherSecretName(pkg.Metadata.Capability)
		if !declared[name] {
			node := merged["node"]
			if node == "" {
				return nil, &TemplateError{
					Package: pkg.Metadata.Name,
					Err:     fmt.Errorf("package publishes household events but has no node param: the publisher secret %s needs a node to live on", name),
				}
			}
			out = append(out, resource.Resource{
				ApiVersion: resource.ApiVersion,
				Kind:       resource.KindSecret,
				Metadata:   resource.Metadata{Name: name, Owner: wantOwner, Node: node},
				Secret:     &resource.SecretSpec{Name: name, Provided: true},
			})
		}
	}

	return out, nil
}

// PublisherSecretName is the conventional name of the secret holding a
// capability's publisher token (ADR-W024 A10): what Expand synthesises
// for a package that publishes, and what `POST
// /api/v1/household/publishers/{capability}` writes the minted token to.
func PublisherSecretName(capability string) string {
	return capability + "-publisher"
}

// templateContext is the small, explicit context every template renders
// over (ADR-W005 A2): .Params is the merged (required+defaulted) param map;
// .Resolved carries dependency-derived outputs — see buildResolved for the
// exact key set.
type templateContext struct {
	Params   map[string]string
	Resolved map[string]string
}

func mergeParams(pkg *pkgspec.Package, params map[string]string) (map[string]string, error) {
	declared := map[string]pkgspec.ParamDecl{}
	for _, p := range pkg.Params {
		declared[p.Name] = p
	}

	for name := range params {
		if _, ok := declared[name]; !ok {
			return nil, &MissingParamError{Package: pkg.Metadata.Name, Param: name, Unknown: true}
		}
	}

	merged := map[string]string{}
	for _, decl := range pkg.Params {
		if v, ok := params[decl.Name]; ok {
			merged[decl.Name] = v
			continue
		}
		if decl.Required {
			return nil, &MissingParamError{Package: pkg.Metadata.Name, Param: decl.Name}
		}
		merged[decl.Name] = decl.Default
	}

	// Param values are interpolated straight into resource YAML by
	// renderTemplate. A value carrying a newline or control character could
	// rewrite the document's structure — inject an extra mount, env var, or
	// port, or override metadata.owner — regardless of how the template
	// quotes the scalar. The owner check downstream catches only an owner
	// override that wins the YAML merge, not these. Reject the structural
	// injection vector at the source; legitimate homelab param values
	// (domains, subdomains, ids) never contain control characters.
	for name, v := range merged {
		if i := strings.IndexFunc(v, isYAMLHostileRune); i >= 0 {
			return nil, &TemplateError{
				Package: pkg.Metadata.Name,
				Err: fmt.Errorf("param %q value contains a disallowed control character at offset %d "+
					"(newlines/control chars are refused to prevent YAML structure injection)", name, i),
			}
		}
	}

	return merged, nil
}

// isYAMLHostileRune reports whether r would let a param value break out of
// its scalar position when interpolated into resource YAML: any newline,
// carriage return, or other C0/C1 control character.
func isYAMLHostileRune(r rune) bool {
	return r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f
}

// buildResolved constructs the .Resolved map exposed to templates. It has
// two halves, and keystone/trait is the written form of both.
//
// INTRINSIC keys (trait.Intrinsic) come from the package itself or from
// the node's own state, so every package gets them whatever it requires:
//
//   - OidcSecretRef: "secret/<capability>-oidc" for this package's own
//     capability, the conventional secret reference a package's OidcClient
//     template should use for SecretRef.
//   - PublisherSecretRef: "secret/<capability>-publisher", the token a
//     package that publishes household events presents to the intake
//     (ADR-W024 A10); put it in the shim's env.
//   - TailnetHost: the node's MagicDNS name under the tailnet posture,
//     empty otherwise. Always present, so a template may test it without
//     tripping missingkey=error. It exists for the packages that must
//     name the host they will be REACHED at rather than the one they
//     declare — Homepage refuses a request whose Host it was not told
//     about, and under this posture that Host is the tailnet name.
//
// TRAIT-CARRIED keys come from the `exports` of packages in the catalog.
// IssuerURL, SessionDomain and DashboardURL used to be computed right
// here, from a hardcoded lookup of "identity"/"oidc" — which meant a
// second trait wanting to hand a consumer anything was a patch to this
// function rather than a package someone wrote. They now live in
// packages/identity's own `provides` block, and this function no longer
// knows what identity is.
//
// The values are unchanged, including the two subtleties that are now the
// identity package's to state:
//
//   - an origin is where a Route is SERVED, not where it was declared.
//     Under the tailnet posture every Route shares the node's one
//     certified name and is told apart by port (ADR-W019 A10); an OIDC
//     issuer must match what the browser saw byte for byte, and deriving
//     it separately from what is actually served is how you get a login
//     loop that names nothing.
//   - a session domain cannot be a public suffix or a parent of one.
//     ts.net is one, so the tailnet posture scopes the cookie to the full
//     node name rather than to any parent of it.
//
// RESOLUTION ORDER, stated because a package author will eventually meet
// it: intrinsic keys are written first and cannot be overridden, and among
// providers the first in catalog order to export a key keeps it. Catalog
// order is the caller's — keystone/api builds it from what the deployment
// has enabled — so this is the same "first provider wins" rule the old
// hardcoded lookup had.
//
// An export that renders EMPTY is not published, so a consumer referencing
// it fails loudly at its own template rather than receiving a half-built
// URL. A portal origin derived from a domain nobody has supplied yet is
// not a better answer than no answer.
func buildResolved(pkg *pkgspec.Package, params map[string]string, catalog *Catalog) (map[string]string, error) {
	var current []resource.Resource
	var pkgs []*pkgspec.Package
	if catalog != nil {
		current = catalog.current
		pkgs = catalog.pkgs
	}
	posture := ingressorigin.Of(current)

	resolved := map[string]string{
		"OidcSecretRef":      "secret/" + pkg.Metadata.Capability + "-oidc",
		"PublisherSecretRef": "secret/" + PublisherSecretName(pkg.Metadata.Capability),
		"TailnetHost":        posture.TailnetName,
	}

	funcs := exportFuncs(posture)
	for _, provider := range pkgs {
		var ctx *exportContext // built lazily: most packages export nothing
		for _, prov := range provider.Provides {
			for _, key := range sortedKeys(prov.Exports) {
				if _, taken := resolved[key]; taken {
					continue
				}
				if ctx == nil {
					ctx = &exportContext{Params: exportParams(provider, params)}
				}
				value, err := renderExport(provider.Metadata.Name, key, prov.Exports[key], *ctx, funcs)
				if err != nil {
					return nil, err
				}
				if value == "" {
					continue
				}
				resolved[key] = value
			}
		}
	}

	return resolved, nil
}

// exportContext is what an export template renders over. Deliberately
// only .Params: an export answers "what do I hand my consumers", and the
// only inputs to that are what the provider was configured with and where
// this deployment serves things — the latter arriving as functions rather
// than as data, because it is computed rather than declared.
type exportContext struct {
	Params map[string]string
}

// exportFuncs binds trait.ExportFuncs to a posture.
//
// Every function is total — it takes `any`, stringifies, and cannot fail
// — so a rendered export cannot error on a type. That matters because the
// alternative is an install that fails after approval, while a plan is
// being expanded, with a message about a template rather than about the
// package that shipped it.
//
// TestExportFuncsMatchTheVocabulary pins this map equal to
// trait.ExportFuncs. A name declared there and not bound here would parse
// at lint and fail at expansion, which is exactly the split between "the
// vocabulary" and "the engine" that this design exists to prevent.
func exportFuncs(p ingressorigin.Posture) template.FuncMap {
	return template.FuncMap{
		"origin":        func(host any) string { return p.OriginFor(fmt.Sprint(host)) },
		"sessionDomain": func(domain any) string { return p.SessionDomain(fmt.Sprint(domain)) },
	}
}

// exportParams are the params an export renders over: the requesting
// package's params, overlaid by the PROVIDER's declared defaults.
//
// The overlay direction preserves what the hardcoded lookup did (provider
// default first, requesting package's param as the fallback) and is worth
// naming as the spike-scoped compromise it is: a Catalog holds packages as
// DECLARED, not the params a deployment actually enabled them with, so a
// provider's default is the only provider-side value available here. For
// the one value this has ever carried it makes no practical difference —
// public_domain is required and undefaulted, so the requesting package's
// own value is what lands — but a provider that adds a defaulted param
// will see that default win, and should expect to.
func exportParams(provider *pkgspec.Package, requesting map[string]string) map[string]string {
	merged := make(map[string]string, len(requesting)+len(provider.Params))
	for k, v := range requesting {
		merged[k] = v
	}
	for _, decl := range provider.Params {
		if decl.Default != "" {
			merged[decl.Name] = decl.Default
		}
	}
	return merged
}

// renderExport renders one export value. missingkey=zero rather than
// error: pkgspec already refuses an export naming a param its own package
// does not declare, so a key absent here means an optional param with no
// default, and the empty string is the honest answer — one an export
// written with `with` handles by publishing nothing.
func renderExport(pkgName, key, expr string, ctx exportContext, funcs template.FuncMap) (string, error) {
	t, err := template.New(pkgName + ".exports." + key).Funcs(funcs).Option("missingkey=zero").Parse(expr)
	if err != nil {
		return "", &TemplateError{Package: pkgName, Err: fmt.Errorf("exports.%s: parsing: %w", key, err)}
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", &TemplateError{Package: pkgName, Err: fmt.Errorf("exports.%s: executing: %w", key, err)}
	}
	return strings.TrimSpace(buf.String()), nil
}

// sortedKeys makes export evaluation order deterministic. Map order would
// otherwise decide which of two providers of a key wins on any given run,
// and a .Resolved that varies between two renders of identical inputs is a
// reconciler that sees a change where none happened.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func renderTemplate(pkgName string, index int, tmplStr string, ctx templateContext) ([]byte, error) {
	t, err := template.New(fmt.Sprintf("%s[%d]", pkgName, index)).Option("missingkey=error").Parse(tmplStr)
	if err != nil {
		return nil, &TemplateError{Package: pkgName, Index: index, Err: fmt.Errorf("parsing template: %w", err)}
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return nil, &TemplateError{Package: pkgName, Index: index, Err: fmt.Errorf("executing template: %w", err)}
	}
	rendered := buf.String()
	if strings.TrimSpace(rendered) == "" {
		return nil, &TemplateError{Package: pkgName, Index: index, Err: fmt.Errorf("rendered to empty document")}
	}
	return buf.Bytes(), nil
}
