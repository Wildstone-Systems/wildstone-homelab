// Package catalog is the set of packages Keystone can install.
//
// It exists because expansion moved into Keystone (ADR-W021 A1). The CLI
// used to load a package directory, render it locally, and POST the
// resulting documents; Keystone never saw a package, so the Console had
// nothing to call, plans recorded consequences rather than intent, and
// `requires` resolved against whatever directories the operator happened
// to pass (F40).
//
// A catalog is data, like packages and the registry index are. This is
// the directory source — the development affordance and, once the
// release bundles its packages, the shipped one too. A registry source
// (ADR-W010 A3, signed and fetched) is additive and not here yet.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
)

// Catalog holds loaded packages, keyed by name. Versions are kept as a
// list per name so a future upgrade path can choose between them; today
// a name resolves to exactly one.
type Catalog struct {
	byName map[string][]*pkgspec.Package
}

// LoadDir reads every immediate subdirectory of root that contains a
// package.yaml. A subdirectory without one is skipped rather than
// failing: a catalog directory is allowed to hold a README.
//
// A package that fails to parse IS an error. Silently omitting it would
// make "the package is not in the catalog" the symptom of a typo, which
// is a considerably worse thing to debug than a refusal to start.
func LoadDir(root string) (*Catalog, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading %s: %w", root, err)
	}
	c := &Catalog{byName: map[string][]*pkgspec.Package{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifest := filepath.Join(root, e.Name(), "package.yaml")
		if _, err := os.Stat(manifest); err != nil {
			continue
		}
		pkg, err := pkgspec.Load(manifest)
		if err != nil {
			return nil, fmt.Errorf("catalog: %s: %w", manifest, err)
		}
		c.byName[pkg.Metadata.Name] = append(c.byName[pkg.Metadata.Name], pkg)
	}
	return c, nil
}

// List returns every package, ordered by name then version, so the
// Console renders a stable list and tests can assert one.
func (c *Catalog) List() []*pkgspec.Package {
	if c == nil {
		return nil
	}
	var out []*pkgspec.Package
	for _, pkgs := range c.byName {
		out = append(out, pkgs...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Metadata.Name != out[j].Metadata.Name {
			return out[i].Metadata.Name < out[j].Metadata.Name
		}
		return out[i].Metadata.Version < out[j].Metadata.Version
	})
	return out
}

// Get resolves a package by name, and by version when one is given. An
// empty version means "the only one", and is an error when the catalog
// holds several — guessing which version an operator meant is not a
// service worth providing.
func (c *Catalog) Get(name, version string) (*pkgspec.Package, error) {
	if c == nil {
		return nil, fmt.Errorf("no catalog is configured")
	}
	pkgs := c.byName[name]
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("package %q is not in the catalog", name)
	}
	if version == "" {
		if len(pkgs) > 1 {
			return nil, fmt.Errorf("package %q has %d versions in the catalog; name one", name, len(pkgs))
		}
		return pkgs[0], nil
	}
	for _, p := range pkgs {
		if p.Metadata.Version == version {
			return p, nil
		}
	}
	return nil, fmt.Errorf("package %q has no version %q in the catalog", name, version)
}

// ProvidersFor returns the catalog packages whose capability is in
// enabled, which is how `requires` resolves against the deployment
// rather than against directories someone passed on a command line
// (ADR-W021 A4, closing F40).
func (c *Catalog) ProvidersFor(enabled map[string]bool) []*pkgspec.Package {
	if c == nil {
		return nil
	}
	var out []*pkgspec.Package
	for _, p := range c.List() {
		if enabled[p.Metadata.Capability] {
			out = append(out, p)
		}
	}
	return out
}
