package main

// `wild capability` (ADR-W015 A2): enable expands a package LOCALLY —
// the exact engine lint and Keystone use — and proposes the rendered
// resources through POST /plans, so policy, risk grading, and approval
// apply unchanged; there is no side door. disable proposes deletion of
// everything the capability owns.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func runCapability(args []string) int {
	if len(args) < 1 {
		capabilityUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	target, rest := splitLeadingArg(rest)
	if target == "" {
		fmt.Fprintf(os.Stderr, "wild capability %s: target required\n", sub)
		capabilityUsage()
		return 2
	}

	fs := flag.NewFlagSet("wild capability "+sub, flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	var sets, catalogs multiFlag
	fs.Var(&sets, "set", "param as name=value (repeatable; enable only)")
	fs.Var(&catalogs, "catalog", "package dir satisfying a require (repeatable; enable only)")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("capability")
	if !ok {
		return 2
	}

	switch sub {
	case "enable":
		return capabilityEnable(*apiBase, token, target, sets, catalogs)
	case "disable":
		return capabilityDisable(*apiBase, token, target)
	default:
		capabilityUsage()
		return 2
	}
}

func capabilityEnable(apiBase, token, pkgDir string, sets, catalogs []string) int {
	pkg, err := pkgspec.Load(filepath.Join(pkgDir, "package.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild capability enable:", err)
		return 1
	}
	params := map[string]string{}
	for _, kv := range sets {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			fmt.Fprintf(os.Stderr, "wild capability enable: -set %q must be name=value\n", kv)
			return 2
		}
		params[k] = v
	}
	catalogPkgs := []*pkgspec.Package{pkg}
	for _, cdir := range catalogs {
		cpkg, err := pkgspec.Load(filepath.Join(cdir, "package.yaml"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild capability enable: catalog:", err)
			return 1
		}
		catalogPkgs = append(catalogPkgs, cpkg)
	}

	rendered, err := capability.NewEngine().Expand(pkg, params, capability.NewCatalog(catalogPkgs...))
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild capability enable: expand:", err)
		return 1
	}
	docs := make([]string, 0, len(rendered))
	for _, res := range rendered {
		data, err := resource.Marshal(res)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wild capability enable:", err)
			return 1
		}
		docs = append(docs, string(data))
	}
	body := map[string]any{
		"summary": fmt.Sprintf("enable capability %s (package %s %s)",
			pkg.Metadata.Capability, pkg.Metadata.Name, pkg.Metadata.Version),
		"put": docs,
	}
	return simpleCall("capability", apiBase, token, http.MethodPost, "/api/v1/plans", body)
}

func capabilityDisable(apiBase, token, name string) int {
	raw, err := apiCall(apiBase, token, http.MethodGet, "/api/v1/resources", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild capability disable:", err)
		return 1
	}
	var views []struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal([]byte(raw), &views); err != nil {
		fmt.Fprintln(os.Stderr, "wild capability disable: parsing resources:", err)
		return 1
	}
	owner := "capability/" + name
	var deletes []map[string]string
	for _, v := range views {
		if v.Owner == owner {
			deletes = append(deletes, map[string]string{"kind": v.Kind, "name": v.Name})
		}
	}
	if len(deletes) == 0 {
		fmt.Fprintf(os.Stderr, "wild capability disable: no resources owned by %s\n", owner)
		return 1
	}
	body := map[string]any{
		"summary": fmt.Sprintf("disable capability %s (%d resource(s) removed)", name, len(deletes)),
		"delete":  deletes,
	}
	return simpleCall("capability", apiBase, token, http.MethodPost, "/api/v1/plans", body)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func capabilityUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild capability enable <pkg-dir> [-set k=v ...] [-catalog dir ...] [-api URL]
  wild capability disable <capability-name> [-api URL]
The proposal lands as an ordinary plan: review and approve it (wild plan).`)
}
