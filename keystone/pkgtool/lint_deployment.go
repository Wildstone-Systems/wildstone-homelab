package pkgtool

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// deploymentIssues checks a package's `devices` and `deployment` fields
// against what it renders (the package deployment capabilities contract,
// §4–§5, rule numbers below). It renders more than the defaults: one
// variant with every labelled device param filled, and one per value of
// each `enum:` param, because a device or host networking a param
// switches on is exactly what a defaults-only check misses (Frigate with
// a Coral).
//
// Opt-in for now: a package that declares neither field is not checked.
// The fields are new to the format and the parser rejects unknown
// fields, so catalog packages adopt them a release after this lint ships
// (contract §7); enforcing rule 1 on every package before then would fail
// the whole catalog.
func deploymentIssues(pkg *pkgspec.Package, base map[string]string, catalog *capability.Catalog) []Issue {
	if len(pkg.Devices) == 0 && pkg.Deployment == nil {
		return nil
	}
	issues := deploymentVocabularyIssues(pkg)

	byParam := map[string]pkgspec.DeviceDecl{}
	byPath := map[string]pkgspec.DeviceDecl{}
	for _, d := range pkg.Devices {
		if d.Param != "" {
			byParam[d.Param] = d
		} else {
			byPath[d.Path] = d
		}
	}
	dirParams := map[string]bool{}
	for _, decl := range pkg.Params {
		if decl.Source == "directory" {
			dirParams[decl.Name] = true
		}
	}

	variants := deploymentVariants(pkg, base, byParam, dirParams)
	needs := map[string]*componentNeeds{}
	var order []string
	matched := map[string]bool{}
	declared := declaredComponents(pkg)
	for _, v := range variants {
		rendered, err := capability.NewEngine().Expand(pkg, v.params, catalog)
		if err != nil {
			// The defaults variant's failure is already the expand issue;
			// another variant failing is worth saying, since it is a
			// combination a household can choose.
			if v.name != "defaults" {
				issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf("the %s variant does not render: %v", v.name, err)})
			}
			continue
		}
		for name := range declared {
			for _, r := range rendered {
				if r.Kind == resource.KindContainer && r.Metadata.Name == renderName(name, v.params) {
					matched[name] = true
				}
			}
		}
		for _, r := range rendered {
			if r.Kind != resource.KindContainer || r.Container == nil {
				continue
			}
			n, ok := needs[r.Metadata.Name]
			if !ok {
				n = &componentNeeds{decl: componentDecl(declared, r.Metadata.Name, v.params)}
				needs[r.Metadata.Name] = n
				order = append(order, r.Metadata.Name)
			}
			n.observe(r.Container, v, byParam, byPath, dirParams)
		}
	}

	// Rule 8: a declared component must exist in at least one variant.
	for _, name := range sortedKeys(declared) {
		if !matched[name] {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
				"deployment.components %q matches no rendered Container in any variant", name)})
		}
	}

	hasMain := false
	sort.Strings(order)
	for _, name := range order {
		n := needs[name]
		c := n.decl
		where := "Container/" + name
		if c.Role != pkgspec.RoleWorker {
			hasMain = true
		}
		for _, d := range n.unlabelled {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
				"%s maps %s, which no devices entry labels: add it under devices with kind identity, class or any-node", where, d)})
		}
		if c.Relocate {
			for _, d := range n.identity {
				issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
					"%s declares relocate: true but maps %s, a specific device, which is on one machine only", where, d)})
			}
			for _, p := range n.fixedFolders {
				issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
					"%s declares relocate: true but mounts the fixed host folder %s, which is on one machine's disk only; take the folder from a source: directory param so it can be on the household's share", where, p)})
			}
			if n.hostNetwork {
				issues = append(issues, Issue{Rule: "deployment", Warning: true, Msg: fmt.Sprintf(
					"%s declares relocate: true and uses the host's network: check it does not rely on LAN discovery or a port only one machine has", where)})
			}
		}
		if c.Copies == pkgspec.CopiesMany {
			for _, m := range n.writes {
				issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
					"%s declares copies: many but writes %s: copies cannot share that state", where, m)})
			}
			if n.hostNetwork {
				issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
					"%s declares copies: many but uses the host's network: two copies would want the same ports", where)})
			}
			for _, d := range n.identity {
				issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
					"%s declares copies: many but maps %s, a specific device only one copy can hold", where, d)})
			}
		}
	}
	// Rule 7: workers do work for a main component.
	if len(order) > 0 && !hasMain {
		issues = append(issues, Issue{Rule: "deployment", Msg: "every component is a worker: a worker needs a main component in the same package"})
	}

	return append(issues, freeTextNeedsIssues(pkg, base, catalog, byParam, dirParams)...)
}

// deploymentVocabularyIssues holds the fields to their closed sets.
func deploymentVocabularyIssues(pkg *pkgspec.Package) []Issue {
	var issues []Issue
	seen := map[string]bool{}
	for _, d := range pkg.Devices {
		key := "param " + d.Param
		if d.Path != "" {
			key = "path " + d.Path
		}
		if seen[key] {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf("devices: %s is labelled twice", key)})
		}
		seen[key] = true
		if !pkgspec.DeviceKinds[d.Kind] {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
				"devices: %s has kind %q (one of: %s)", key, d.Kind, strings.Join(sortedKeys(pkgspec.DeviceKinds), ", "))})
		}
		if (d.Kind == pkgspec.DeviceClass) != (d.Class != "") {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
				"devices: %s: class names the node label a kind: class device needs, and only that kind has one", key)})
		}
	}
	if pkg.Deployment == nil {
		return issues
	}
	names := map[string]bool{}
	for _, c := range pkg.Deployment.Components {
		if names[c.Name] {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf("deployment.components %q is listed twice", c.Name)})
		}
		names[c.Name] = true
		if c.Copies != "" && !pkgspec.ComponentCopies[c.Copies] {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
				"deployment.components %q: copies %q (one of: %s)", c.Name, c.Copies, strings.Join(sortedKeys(pkgspec.ComponentCopies), ", "))})
		}
		if c.Role != "" && !pkgspec.ComponentRoles[c.Role] {
			issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
				"deployment.components %q: role %q (one of: %s)", c.Name, c.Role, strings.Join(sortedKeys(pkgspec.ComponentRoles), ", "))})
		}
	}
	return issues
}

type deploymentVariant struct {
	name   string
	params map[string]string
}

// deploymentVariants is the bounded set lint renders (contract §5):
// defaults, every labelled device param filled, and each enum value one
// at a time. Every variant fills `source: directory` params with a
// placeholder, so a folder that came from one can be told apart from a
// path written in the template.
func deploymentVariants(pkg *pkgspec.Package, base map[string]string, byParam map[string]pkgspec.DeviceDecl, dirParams map[string]bool) []deploymentVariant {
	with := func(extra map[string]string) map[string]string {
		// Defaults are written out, so a device a default names is
		// attributed to its param like any other value.
		p := map[string]string{}
		for _, decl := range pkg.Params {
			if decl.Default != "" {
				p[decl.Name] = decl.Default
			}
		}
		for k, v := range base {
			p[k] = v
		}
		for name := range dirParams {
			p[name] = dirPlaceholder(name)
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	out := []deploymentVariant{{name: "defaults", params: with(nil)}}
	if len(byParam) > 0 {
		devices := map[string]string{}
		for name := range byParam {
			devices[name] = devicePlaceholder(name)
		}
		out = append(out, deploymentVariant{name: "every-device", params: with(devices)})
	}
	for _, decl := range pkg.Params {
		values, ok := decl.EnumValues()
		if !ok {
			continue
		}
		for _, v := range values {
			out = append(out, deploymentVariant{name: decl.Name + "=" + v, params: with(map[string]string{decl.Name: v})})
		}
	}
	return out
}

// Placeholders are allowlisted shapes (a serial by-id path; a folder under
// the managed prefix), so they render, and unique per param, so a mapping
// says which param produced it.
func devicePlaceholder(param string) string { return "/dev/serial/by-id/wildstone-lint-" + param }
func dirPlaceholder(param string) string    { return "/srv/wildstone/wildstone-lint-" + param }

// componentNeeds accumulates one Container's needs across variants.
type componentNeeds struct {
	decl         pkgspec.ComponentDecl
	identity     []string
	unlabelled   []string
	fixedFolders []string
	writes       []string
	hostNetwork  bool
}

func (n *componentNeeds) observe(spec *resource.ContainerSpec, v deploymentVariant, byParam, byPath map[string]pkgspec.DeviceDecl, dirParams map[string]bool) {
	for _, dev := range spec.Devices {
		decl, ok := byPath[dev.Source]
		label := dev.Source
		if !ok {
			for name, d := range byParam {
				if v.params[name] == dev.Source {
					decl, ok, label = d, true, "the device from param "+name
				}
			}
		}
		switch {
		case !ok:
			n.unlabelled = appendOnce(n.unlabelled, dev.Source)
		case decl.Kind == pkgspec.DeviceIdentity:
			n.identity = appendOnce(n.identity, label)
		}
	}
	if spec.HostNetwork() {
		n.hostNetwork = true
	}
	for _, m := range spec.Mounts {
		if !m.ReadOnly {
			n.writes = appendOnce(n.writes, m.Source)
		}
		if !strings.HasPrefix(m.Source, "/") || isManagedConfig(m.Source) || fromDirParam(m.Source, v.params, dirParams) {
			continue
		}
		n.fixedFolders = appendOnce(n.fixedFolders, m.Source)
	}
}

// isManagedConfig is Keystone's own rendered config, which it can deliver
// to any machine (contract rule 3's exception).
func isManagedConfig(path string) bool {
	return path == "/etc/wildstone" || strings.HasPrefix(path, "/etc/wildstone/")
}

func fromDirParam(path string, params map[string]string, dirParams map[string]bool) bool {
	for name := range dirParams {
		p := params[name]
		if p != "" && (path == p || strings.HasPrefix(path, p+"/")) {
			return true
		}
	}
	return false
}

// freeTextNeedsIssues is rule 10: a free-text param may not change what a
// component needs. Each optional param that is not a labelled device, a
// directory or an enum is set to a probe value; if the rendered needs
// differ from the defaults', the param must become one of those.
func freeTextNeedsIssues(pkg *pkgspec.Package, base map[string]string, catalog *capability.Catalog, byParam map[string]pkgspec.DeviceDecl, dirParams map[string]bool) []Issue {
	baseline, err := needsFingerprint(pkg, base, catalog)
	if err != nil {
		return nil
	}
	var issues []Issue
	for _, decl := range pkg.Params {
		if _, isEnum := decl.EnumValues(); isEnum || decl.Required || byParam[decl.Name].Kind != "" || dirParams[decl.Name] ||
			decl.Name == pkgspec.InstanceParam || decl.Source != "" {
			continue
		}
		probe := decl.Example
		if probe == "" {
			probe = "lint-probe"
		}
		if probe == decl.Default {
			continue
		}
		p := map[string]string{}
		for k, v := range base {
			p[k] = v
		}
		p[decl.Name] = probe
		got, err := needsFingerprint(pkg, p, catalog)
		if err != nil || got == baseline {
			continue
		}
		issues = append(issues, Issue{Rule: "deployment", Msg: fmt.Sprintf(
			"param %q is free text but changes what the package needs from a machine (a device, host networking or a host folder): "+
				"make it an enum: param, or label it under devices", decl.Name)})
	}
	return issues
}

// needsFingerprint renders params and summarizes every Container's
// devices, host networking and host folders, in a stable order.
func needsFingerprint(pkg *pkgspec.Package, params map[string]string, catalog *capability.Catalog) (string, error) {
	rendered, err := capability.NewEngine().Expand(pkg, params, catalog)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, r := range rendered {
		if r.Kind != resource.KindContainer || r.Container == nil {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s host=%t", r.Metadata.Name, r.Container.HostNetwork())
		for _, d := range r.Container.Devices {
			fmt.Fprintf(&b, " dev=%s", d.Source)
		}
		for _, m := range r.Container.Mounts {
			if strings.HasPrefix(m.Source, "/") && !isManagedConfig(m.Source) {
				fmt.Fprintf(&b, " dir=%s", m.Source)
			}
		}
		lines = append(lines, b.String())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), nil
}

func declaredComponents(pkg *pkgspec.Package) map[string]pkgspec.ComponentDecl {
	out := map[string]pkgspec.ComponentDecl{}
	if pkg.Deployment != nil {
		for _, c := range pkg.Deployment.Components {
			out[c.Name] = c
		}
	}
	return out
}

// componentDecl finds the declaration for a rendered Container name, or
// the defaults.
func componentDecl(declared map[string]pkgspec.ComponentDecl, rendered string, params map[string]string) pkgspec.ComponentDecl {
	for name, c := range declared {
		if renderName(name, params) == rendered {
			return c
		}
	}
	return pkgspec.ComponentDecl{Name: rendered}
}

// renderName renders a component name's template expressions over
// .Params. A name that does not parse is compared as written.
func renderName(name string, params map[string]string) string {
	if !strings.Contains(name, "{{") {
		return name
	}
	t, err := template.New("name").Option("missingkey=error").Parse(name)
	if err != nil {
		return name
	}
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]any{"Params": params}); err != nil {
		return name
	}
	return b.String()
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
