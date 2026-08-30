// Package pkgtool is the E5 package toolchain (ADR-W010): lint as the
// package contract test, deterministic signed archives, and the
// directory-registry format with semver resolution. It is a library
// first; `wild pkg ...` and CI are thin callers.
package pkgtool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
)

// Issue is one lint finding. Lint returns findings rather than stopping
// at the first failure — a package author fixes a batch at a time.
type Issue struct {
	Rule string // "spec", "expand", "policy"
	Msg  string
}

func (i Issue) String() string { return i.Rule + ": " + i.Msg }

// LintOptions configures a lint run.
type LintOptions struct {
	// Fillers supply values for required params during the dry-run
	// Expand. Any required param not covered is auto-filled with
	// "lint-<name>" (name-regexp-safe), so a package lints without the
	// author inventing throwaway values; pass real-shaped fillers when a
	// template is sensitive to the value's shape.
	Fillers map[string]string
	// CatalogDirs name package directories whose Provides satisfy this
	// package's requires (e.g. lint photos-immich with the identity
	// package on the catalog). Structural traits (ADR-W010 A1) never
	// need an entry.
	CatalogDirs []string
}

// Lint runs the ADR-W010 A5 contract test over the package at dir
// (containing package.yaml), never touching any host: parse + validate,
// dry-run Expand (which enforces resource validation and digest pins on
// every rendered resource), and the E4 policy engine over the rendered
// tree — a package that ships a policy violation fails lint, not the
// user's install. The returned error is reserved for I/O-level failures;
// package problems come back as Issues.
func Lint(dir string, opts LintOptions) ([]Issue, error) {
	pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
	if err != nil {
		if isIOError(err) {
			return nil, err // filesystem problem: the caller's error, not the package's
		}
		return []Issue{{Rule: "spec", Msg: err.Error()}}, nil
	}

	catalogPkgs := []*pkgspec.Package{pkg}
	for _, cdir := range opts.CatalogDirs {
		cpkg, err := pkgspec.Load(filepath.Join(cdir, "package.yaml"))
		if err != nil {
			return nil, fmt.Errorf("pkgtool: loading catalog package %s: %w", cdir, err)
		}
		catalogPkgs = append(catalogPkgs, cpkg)
	}
	catalog := capability.NewCatalog(catalogPkgs...)

	params := map[string]string{}
	for _, decl := range pkg.Params {
		if !decl.Required {
			continue
		}
		if v, ok := opts.Fillers[decl.Name]; ok {
			params[decl.Name] = v
		} else {
			params[decl.Name] = "lint-" + sanitizeParam(decl.Name)
		}
	}

	rendered, err := capability.NewEngine().Expand(pkg, params, catalog)
	if err != nil {
		return []Issue{{Rule: "expand", Msg: err.Error()}}, nil
	}

	var issues []Issue
	// A package's declared classifications must be in the closed policy
	// vocabulary. The field was parsed and read nowhere, so
	// ingress-tailnet shipped `classifications: [admin]` — not a value
	// that exists; admin_tailscale was meant — and lint said the package
	// was clean (F31). Inert while nothing consults the field, and a trap
	// the day something does, which is exactly when a wrong value is
	// hardest to notice.
	for _, c := range pkg.Policies.Classifications {
		if !policy.Classifications[c] {
			issues = append(issues, Issue{
				Rule: "classification",
				Msg: fmt.Sprintf("policies.classifications: %q is not in the closed vocabulary (%s)",
					c, strings.Join(policy.ClassificationNames(), ", ")),
			})
		}
	}
	violations, exempted := policy.NewEngine(nil).Validate(policy.Change{
		Proposed:  rendered,
		Principal: "pkgtool/lint",
	})
	_ = exempted // lint runs with no exemptions by construction
	for _, v := range violations {
		issues = append(issues, Issue{Rule: "policy", Msg: v.String()})
	}
	return issues, nil
}

// sanitizeParam maps a param name into the lowercase [a-z0-9-] alphabet
// so an auto-filler can never itself trip resource-name validation.
func sanitizeParam(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// isIOError distinguishes filesystem problems (caller's error) from
// package content problems (lint issues).
func isIOError(err error) bool {
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}
