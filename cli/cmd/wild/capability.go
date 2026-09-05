package main

// `wild capability` (ADR-W015 A2): enable names a package in KEYSTONE's
// catalog and POSTs to /api/v1/capabilities, which expands it there and
// proposes the result as an ordinary plan — so policy, risk grading and
// approval apply unchanged; there is no side door. disable proposes
// deletion of everything the capability owns.
//
// Expansion used to happen HERE, from a package directory on the
// operator's disk. That meant Keystone never saw a package: the Console
// had nothing to call, the stored plan recorded rendered documents
// rather than what was installed, and `requires` resolved against
// whatever -catalog directories were passed instead of against what the
// deployment actually has enabled (F40). ADR-W021 A1 moved it; this file
// became a client of the same endpoint the Console uses, which is what
// ONE API was for.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	fs.Var(&catalogs, "catalog", "ignored; requires now resolve against what Keystone has enabled (ADR-W021 A4)")
	version := fs.String("version", "", "package version; omit when the catalog holds one (enable only)")
	dryRun := fs.Bool("dry-run", false, "render and check without proposing: prints the plan and the secrets you must supply (enable only)")
	fs.Parse(rest) //nolint:errcheck // ExitOnError
	token, ok := requireToken("capability")
	if !ok {
		return 2
	}

	switch sub {
	case "enable":
		return capabilityEnable(*apiBase, token, target, *version, sets, catalogs, *dryRun)
	case "disable":
		return capabilityDisable(*apiBase, token, target)
	default:
		capabilityUsage()
		return 2
	}
}

func capabilityEnable(apiBase, token, target, version string, sets, catalogs multiFlag, dryRun bool) int {
	// A path is what this command used to take, so say what changed
	// rather than reporting "package not in the catalog" for a directory
	// that plainly exists.
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		fmt.Fprintf(os.Stderr, "wild capability enable: %s is a directory, and expansion now happens in Keystone (ADR-W021 A1).\n", target)
		fmt.Fprintf(os.Stderr, "  Name the package instead:  wild capability enable %s -set ...\n", filepath.Base(target))
		fmt.Fprintln(os.Stderr, "  Keystone finds it in its catalog; point one at a checkout with `keystone -catalog-dir <dir>`.")
		return 2
	}
	if len(catalogs) > 0 {
		fmt.Fprintln(os.Stderr, "wild capability enable: -catalog is ignored; requires resolve against what Keystone has enabled")
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
	body := map[string]any{"package": target, "params": params}
	if version != "" {
		body["version"] = version
	}
	if dryRun {
		body["dryRun"] = true
	}
	return simpleCall("capability", apiBase, token, http.MethodPost, "/api/v1/capabilities", body)
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
  wild capability enable <package> [-set k=v ...] [-version v] [-dry-run] [-api URL]
  wild capability disable <capability-name> [-api URL]
  wild pkg list                                    (what the catalog holds)
The package is named from Keystone's catalog; expansion happens there.
-dry-run renders and checks without proposing, and prints the secrets
only you can supply. The proposal lands as an ordinary plan: review and
approve it (wild plan).`)
}

// runPkgList shows what Keystone can install. Without it the catalog is
// unusable from the CLI: `capability enable` takes a package NAME now,
// and nothing else would tell you which names exist.
func runPkgList(args []string) int {
	fs := flag.NewFlagSet("wild pkg list", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	fs.Parse(args) //nolint:errcheck // ExitOnError
	token, ok := requireToken("pkg list")
	if !ok {
		return 2
	}
	raw, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/packages", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg list:", err)
		return 1
	}
	var pkgs []struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Capability string `json:"capability"`
		Profile    string `json:"profile"`
		Enabled    bool   `json:"enabled"`
		Params     []struct {
			Name     string `json:"name"`
			Required bool   `json:"required"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(raw), &pkgs); err != nil {
		fmt.Fprintln(os.Stderr, "wild pkg list: parsing response:", err)
		return 1
	}
	if len(pkgs) == 0 {
		fmt.Println("the catalog is empty (start keystone with -catalog-dir)")
		return 0
	}
	fmt.Printf("%-22s %-9s %-14s %-9s %s\n", "PACKAGE", "VERSION", "CAPABILITY", "STATE", "REQUIRED PARAMS")
	for _, p := range pkgs {
		var req []string
		for _, prm := range p.Params {
			if prm.Required {
				req = append(req, prm.Name)
			}
		}
		state := "-"
		if p.Enabled {
			state = "enabled"
		}
		fmt.Printf("%-22s %-9s %-14s %-9s %s\n", p.Name, p.Version, p.Capability, state, strings.Join(req, ", "))
	}
	return 0
}
