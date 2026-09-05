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
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
	// Params must not carry secret material (ADR-W021 A2). A param's
	// value is recorded in the plan's Origin, which reaches the runtime
	// DB and the audit trail — so a package that asks for a password
	// through a param has quietly created a second path for secret
	// material, alongside the one designed for it.
	//
	// Name-based and deliberately narrow: a check that cannot see values
	// can only catch the declaration, and a broad match would reject
	// honest names like tunnel_id or ldap_base_dn. Secrets travel as
	// Secret resources — generated on the node, or `provided` and set by
	// the operator.
	for _, decl := range pkg.Params {
		if word := secretishParamWord(decl.Name); word != "" {
			issues = append(issues, Issue{Rule: "params", Msg: fmt.Sprintf(
				"param %q looks like secret material (%q): a param's value is stored in the plan and the audit trail. "+
					"Declare a Secret instead — `provided: true` if only the operator can supply it", decl.Name, word)})
		}
	}
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
	for _, r := range rendered {
		issues = append(issues, containerEnvIssues(r)...)
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

// containerEnvIssues rejects a "container/<name>" reference in a
// Container's env.
//
// "container/<name>:<port>" is real syntax, but only in Route.upstream,
// where the reconciler resolves it (see caddyUpstream). Nothing resolves
// it in env: the agent's materializeEnv substitutes secret/ and passes
// everything else through byte for byte, so the value reaches the process
// literally.
//
// Two shipped packages did this in six places and neither could have
// started. immich-server got DB_HOSTNAME=container/immich-postgres, and
// node-postgres reads a value containing "/" as a UNIX socket path:
//
//	Error: connect ENOENT container/immich-postgres/.s.PGSQL.5432
//
// paperless-ngx had four, three of them with the prefix inside a URL.
// Nothing caught it, because a package's images are never pulled by lint
// and the env is only read by the process that dies.
//
// The fix packages want is the plain name: containers on the Wildstone
// network reach each other by container-name DNS, so "immich-postgres" is
// already the correct value. Refusing the prefix here rather than
// stripping it at apply time keeps the privileged agent's substitution
// rule at exactly one prefix, and puts the error in front of the package
// author instead of in a container log the operator has to go find.
func containerEnvIssues(r resource.Resource) []Issue {
	if r.Kind != resource.KindContainer || r.Container == nil {
		return nil
	}
	keys := make([]string, 0, len(r.Container.Env))
	for k := range r.Container.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map order must not make lint output nondeterministic
	var issues []Issue
	for _, k := range keys {
		v := r.Container.Env[k]
		if !strings.Contains(v, containerRefPrefix) {
			continue
		}
		issues = append(issues, Issue{
			Rule: "container-env-ref",
			Msg: fmt.Sprintf("Container/%s env %s = %q: %q is only resolved in Route.upstream, never in env — "+
				"the agent passes env through literally, so the process receives this string as written. "+
				"Use the plain container name, which is what container-name DNS resolves.",
				r.Metadata.Name, k, v, containerRefPrefix),
		})
	}
	return issues
}

// containerRefPrefix is the Route.upstream reference form, named here so
// the lint rule and the reconciler cannot disagree about the spelling.
const containerRefPrefix = "container/"

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

// secretishParamNames is a CLOSED list, for the same reason every other
// vocabulary in this repo is closed: a heuristic that grows by guessing
// starts rejecting honest names, and nobody can tell whether a refusal
// is a rule or an accident.
var secretishParamNames = []string{
	"password", "passwd", "secret", "token", "credential", "apikey", "api_key", "private_key", "privatekey",
}

// secretishParamWord reports which forbidden word a param name contains,
// or "" if none does.
func secretishParamWord(name string) string {
	lower := strings.ToLower(name)
	for _, w := range secretishParamNames {
		if strings.Contains(lower, w) {
			return w
		}
	}
	return ""
}
