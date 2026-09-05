// Package capability implements the Capability Engine (ADR-W005 A2): given a
// Package (keystone/pkgspec) and a set of params, it validates the params,
// resolves the package's `requires` traits against a Catalog of enabled
// packages, renders the package's templates over a small explicit context,
// and parses+validates the result into keystone/resource.Resource values.
package capability

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
func (c *Catalog) Provides(trait, mode string) bool {
	if c == nil {
		return false
	}
	for _, pkg := range c.pkgs {
		for _, p := range pkg.Provides {
			if p.Trait == trait && p.Mode == mode {
				return true
			}
		}
	}
	return false
}

// providerParams returns the merged params of the first catalog package
// that provides (trait, mode), or nil if none does. Used to derive
// .Resolved values (e.g. IssuerURL) from the providing package's own
// declared param defaults — a small, explicit, spike-scoped mechanism
// (ADR-W005 A2: "keep .Resolved a documented, small, explicit map — don't
// over-engineer").
func (c *Catalog) providerParams(trait, mode string) map[string]string {
	if c == nil {
		return nil
	}
	for _, pkg := range c.pkgs {
		for _, p := range pkg.Provides {
			if p.Trait == trait && p.Mode == mode {
				merged := map[string]string{}
				for _, decl := range pkg.Params {
					if decl.Default != "" {
						merged[decl.Name] = decl.Default
					}
				}
				return merged
			}
		}
	}
	return nil
}

// structuralTraits are requirements the engine satisfies itself rather
// than through the catalog (ADR-W010 A1): a CLOSED list, mirroring the
// closed-registry philosophy — adding one is a reviewed code change, and
// each entry must name the system mechanism that satisfies it.
var structuralTraits = map[string]bool{
	"ingress": true, // reverse-proxy/Route machinery (ADR-W004 A1)
	"storage": true, // declared Volumes (ADR-W004 A1)
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
		if structuralTraits[req.Trait] {
			// ingress/storage are system mechanisms, always satisfiable
			// (ADR-W010 A1 making ADR-W005 A2's prose real): ingress by the
			// reverse-proxy/Route machinery, storage by declared Volumes.
			// They contribute nothing to .Resolved.
			continue
		}
		if !catalog.Provides(req.Trait, req.Mode) {
			return nil, &UnsatisfiedRequirementError{Package: pkg.Metadata.Name, Trait: req.Trait, Mode: req.Mode}
		}
	}

	resolved := buildResolved(pkg, merged, catalog)
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

	return out, nil
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

// buildResolved constructs the .Resolved map exposed to templates. This is
// deliberately small and explicit (ADR-W005 A2 spike scope), documented
// keys only:
//
//   - IssuerURL: the browser-facing origin of the identity provider's
//     portal. Historically "https://auth.{public_domain}", which assumed
//     a Route is served at the host it declares. Under the tailnet
//     posture it is not — every Route shares the node's one certified
//     name and is told apart by port (ADR-W019 A10) — so this is now
//     built by ingressorigin, the same code that renders the Caddy site
//     address. An OIDC issuer is a string that must match what the
//     browser saw byte for byte; deriving it separately from what is
//     actually served is how you get a login loop that names nothing.
//   - TailnetHost: the node's MagicDNS name under the tailnet posture,
//     empty otherwise. Always present, so a template may test it without
//     tripping missingkey=error. It exists for the packages that must
//     name the host they will be REACHED at rather than the one they
//     declare — Homepage refuses a request whose Host it was not told
//     about, and under this posture that Host is the tailnet name.
//   - OidcSecretRef: "secret/<capability>-oidc" for this package's own
//     capability, the conventional secret reference a package's OidcClient
//     template should use for SecretRef.
func buildResolved(pkg *pkgspec.Package, params map[string]string, catalog *Catalog) map[string]string {
	var current []resource.Resource
	if catalog != nil {
		current = catalog.current
	}
	posture := ingressorigin.Of(current)

	resolved := map[string]string{
		"OidcSecretRef": "secret/" + pkg.Metadata.Capability + "-oidc",
		"TailnetHost":   posture.TailnetName,
	}

	if providerParams := catalog.providerParams("identity", "oidc"); providerParams != nil {
		domain := providerParams["public_domain"]
		if domain == "" {
			domain = params["public_domain"]
		}
		if domain != "" {
			// Under every posture but "tailscale" this is unchanged:
			// OriginFor returns the declared host untouched, so the value
			// is the "https://auth.<domain>" it always was.
			resolved["IssuerURL"] = posture.OriginFor("auth." + domain)
		}
	}

	return resolved
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
