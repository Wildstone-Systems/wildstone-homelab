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

	return merged, nil
}

// buildResolved constructs the .Resolved map exposed to templates. This is
// deliberately small and explicit (ADR-W005 A2 spike scope), documented
// keys only:
//
//   - IssuerURL: derived as "https://auth.{public_domain}" from the
//     identity:oidc provider's own params (if the catalog contains one,
//     regardless of whether this package requires identity:oidc — a
//     package may want it without requiring it, though in practice it's
//     only populated when a provider exists).
//   - OidcSecretRef: "secret/<capability>-oidc" for this package's own
//     capability, the conventional secret reference a package's OidcClient
//     template should use for SecretRef.
func buildResolved(pkg *pkgspec.Package, params map[string]string, catalog *Catalog) map[string]string {
	resolved := map[string]string{
		"OidcSecretRef": "secret/" + pkg.Metadata.Capability + "-oidc",
	}

	if providerParams := catalog.providerParams("identity", "oidc"); providerParams != nil {
		domain := providerParams["public_domain"]
		if domain == "" {
			domain = params["public_domain"]
		}
		if domain != "" {
			resolved["IssuerURL"] = "https://auth." + domain
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
