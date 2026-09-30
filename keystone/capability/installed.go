package capability

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// This file is the half of export resolution that asks what a provider
// has INSTALLED, rather than what it declares.
//
// It exists because an export can only see its provider's declared
// defaults (exportParams; ADR-W025 Part B item 4), and some facts a
// consumer needs are neither defaults nor derivable from the consumer:
// the address a broker was told to listen on is a required param the
// operator typed when installing the BROKER. What that param rendered —
// a Container publishing a port on that address, a Secret on that node —
// is in the deployment's desired state, and that is where these answers
// come from.

// Rehearsal returns a Catalog for a context with no deployment behind it:
// `wild pkg lint`, and the tests that render every shipped package.
//
// There, "what has the provider installed" has no answer, and the honest
// alternative to refusing every consumer is to answer from the provider
// rendered with its own declared defaults and examples — exactly the
// values lint already renders the provider itself with. An install never
// uses this: an example address handed to a real consumer is a service
// connecting somewhere that does not exist.
func (c *Catalog) Rehearsal() *Catalog {
	if c == nil {
		return nil
	}
	out := *c
	out.rehearsal = true
	return &out
}

// installedLookup returns a memoized function yielding the resources the
// provider package has in desired state (or, in a rehearsal, renders).
// Lazy because most exports never ask, and a rehearsal pays for a whole
// expansion of the provider.
func (c *Catalog) installedLookup(provider *pkgspec.Package) func() []resource.Resource {
	var done bool
	var res []resource.Resource
	return func() []resource.Resource {
		if !done {
			done = true
			res = c.installed(provider)
		}
		return res
	}
}

func (c *Catalog) installed(provider *pkgspec.Package) []resource.Resource {
	if c == nil {
		return nil
	}
	if c.rehearsal {
		// The provider is rendered against this same catalog, so one that
		// itself requires something — a Z-Wave server whose admin Route
		// needs identity:oidc (#444) — still has a rehearsal answer. It
		// is rendered ONE level deep: the nested catalog answers every
		// installed-state question with nothing, which is what keeps a
		// provider from recursing into itself (or into a provider that
		// requires it back). A provider whose own requirement needs an
		// installed-state answer to render has no rehearsal answer,
		// which is the honest result for a chain nobody has installed.
		if c.rehearsalNested {
			return nil
		}
		nested := *c
		nested.rehearsalNested = true
		rendered, err := NewEngine().Expand(provider, rehearsalParams(provider), &nested)
		if err != nil {
			return nil
		}
		return rendered
	}
	owner := "capability/" + provider.Metadata.Capability
	var out []resource.Resource
	for _, r := range c.current {
		// Package is stamped at expansion; a statefile older than that
		// stamp still carries the owner, which for a non-instanced
		// provider is the same fact.
		if r.Metadata.Package == provider.Metadata.Name ||
			(r.Metadata.Package == "" && r.Metadata.Owner == owner) {
			out = append(out, r)
		}
	}
	return out
}

// rehearsalParams fills a provider's params the way lint does: default,
// else the author's example, else a name-derived stand-in.
func rehearsalParams(pkg *pkgspec.Package) map[string]string {
	params := map[string]string{}
	for _, d := range pkg.Params {
		switch {
		case d.Default != "":
			params[d.Name] = d.Default
		case d.Example != "":
			params[d.Name] = d.Example
		case d.Required:
			params[d.Name] = "rehearsal-" + strings.ReplaceAll(d.Name, "_", "-")
		}
	}
	return params
}

// publishedMapping finds where the named container publishes a TCP port.
// A mapping with no host address (Docker's every-interface default) is
// found, and its HostIP is empty: there is no ONE address to hand a
// consumer, and publishedHost says so by rendering nothing.
func publishedMapping(installed []resource.Resource, container, port string) (resource.PortMapping, bool) {
	want, err := strconv.ParseUint(port, 10, 32)
	if err != nil {
		return resource.PortMapping{}, false
	}
	for _, r := range installed {
		if r.Kind != resource.KindContainer || r.Container == nil || r.Metadata.Name != container {
			continue
		}
		for _, p := range r.Container.Ports {
			m, ok := resource.ParsePortMapping(p)
			if ok && m.Protocol == "tcp" && uint64(m.ContainerPort) == want {
				m.HostIP = strings.TrimSuffix(strings.TrimPrefix(m.HostIP, "["), "]")
				return m, true
			}
		}
	}
	return resource.PortMapping{}, false
}

// SecretPlacementError refuses a consumer that would read a secret its
// provider exported, somewhere that secret will never exist.
//
// Secrets are node-local (ADR-W012): the agent generates one on the node
// its Secret resource names and substitutes it only there. A consumer on
// another node renders fine, plans fine, applies fine — and then its
// container fails to start with "references secret/x, which does not
// exist on this node", or, worse, a broker rejects it forever. Refusing
// at expansion is the only point where the message can name the
// provider, both nodes, and what to do instead.
type SecretPlacementError struct {
	Package  string
	Provider string
	Secret   string
	// Resource is "<Kind>/<name>" of the consumer's resource that reads
	// the secret, and Node where it would read it.
	Resource string
	Node     string
	// ProviderNodes are where the provider generates the secret; empty
	// means it generates no such secret at all.
	ProviderNodes []string
}

func (e *SecretPlacementError) Error() string {
	if len(e.ProviderNodes) == 0 {
		return fmt.Sprintf("capability: package %s: %s reads secret/%s, but %s issues no such secret — "+
			"it declares a credential for each consumer it serves, and this capability is not one of them",
			e.Package, e.Resource, e.Secret, e.Provider)
	}
	return fmt.Sprintf("capability: package %s: %s on node %q reads secret/%s, which %s generates on node %s. "+
		"Secrets are node-local and there is no cross-node credential path yet: install %s on %s, or move %s",
		e.Package, e.Resource, e.Node, e.Secret, e.Provider, quoteList(e.ProviderNodes),
		e.Package, quoteList(e.ProviderNodes), e.Provider)
}

func quoteList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, " or ")
}

// exportedSecretRef matches an export whose whole value is a secret
// reference — the form an app's env or managed file substitutes.
var exportedSecretRef = regexp.MustCompile(`^secret/([A-Za-z0-9][A-Za-z0-9._-]{0,63})$`)

// secretRefInContent mirrors the reconciler's and the agent's token
// (keystone/reconcile/configfile.go), for the same reason each of those
// mirrors the other: a mismatch means one side acts on a reference the
// other would not.
var secretRefInContent = regexp.MustCompile(`(?:pbkdf2:|mosquitto:)?secret/([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)

// checkExportedSecrets holds a consumer to where the secrets its
// providers export actually live.
//
// Generic on purpose: the engine does not know that mqtt:broker's
// MqttPasswordSecretRef is a password. It knows an export's value is a
// secret reference, that the provider's desired state says which node
// generates that secret, and which of the consumer's rendered resources
// read it. That is the whole rule, and any future trait exporting a
// secret reference is held to it without a line here changing.
//
// A provider with nothing installed yet (or a catalog with no deployment
// state) cannot be compared against, and passes: the requirement check
// has already refused a provider that is not enabled at all. In a
// rehearsal only the "issues no such secret" half applies — the nodes
// there are stand-ins, and comparing two stand-ins proves nothing.
func checkExportedSecrets(out []resource.Resource, resolved map[string]string, exportedBy map[string]*pkgspec.Package, catalog *Catalog) error {
	if catalog == nil {
		return nil
	}
	keys := make([]string, 0, len(exportedBy))
	for k := range exportedBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m := exportedSecretRef.FindStringSubmatch(resolved[key])
		if m == nil {
			continue
		}
		name := m[1]
		var readers []resource.Resource
		for _, r := range out {
			if readsSecret(r, name) {
				readers = append(readers, r)
			}
		}
		if len(readers) == 0 {
			continue
		}
		provider := exportedBy[key]
		installed := catalog.installed(provider)
		if len(installed) == 0 {
			continue
		}
		nodes := map[string]bool{}
		for _, r := range installed {
			if r.Kind == resource.KindSecret && r.Secret != nil && r.Secret.Name == name {
				nodes[r.Metadata.Node] = true
			}
		}
		providerNodes := make([]string, 0, len(nodes))
		for n := range nodes {
			providerNodes = append(providerNodes, n)
		}
		sort.Strings(providerNodes)
		for _, r := range readers {
			if len(nodes) > 0 && (catalog.rehearsal || nodes[r.Metadata.Node]) {
				continue
			}
			return &SecretPlacementError{
				Package:       r.Metadata.Package,
				Provider:      fmt.Sprintf("capability %s (package %s)", provider.Metadata.Capability, provider.Metadata.Name),
				Secret:        name,
				Resource:      string(r.Kind) + "/" + r.Metadata.Name,
				Node:          r.Metadata.Node,
				ProviderNodes: providerNodes,
			}
		}
	}
	return nil
}

// readsSecret reports whether a rendered resource makes its node
// substitute secret/<name>: a container's (or a Job's) env value that is
// exactly the reference — the agent substitutes nothing else — or a
// managed file whose content names it in any of its forms.
func readsSecret(r resource.Resource, name string) bool {
	ref := "secret/" + name
	envReads := func(env map[string]string) bool {
		for _, v := range env {
			if strings.TrimSpace(v) == ref {
				return true
			}
		}
		return false
	}
	switch {
	case r.Kind == resource.KindContainer && r.Container != nil:
		return envReads(r.Container.Env)
	case r.Kind == resource.KindJob && r.Job != nil:
		return envReads(r.Job.Run.Env)
	case r.Kind == resource.KindConfigFile && r.ConfigFile != nil:
		for _, m := range secretRefInContent.FindAllStringSubmatch(r.ConfigFile.Content, -1) {
			if m[1] == name {
				return true
			}
		}
	}
	return false
}

// hostNetworkEnv answers the hostNetworkEnv export function: the value
// of one env var on the provider's named container, as installed, if and
// only if that container runs on the host's network. A bridge container
// is not asked — where it listens is what it publishes, which
// publishedHost reads — and a value that names a secret is withheld.
func hostNetworkEnv(installed []resource.Resource, container, key string) string {
	for _, r := range installed {
		if r.Kind != resource.KindContainer || r.Container == nil || r.Metadata.Name != container {
			continue
		}
		if !r.Container.HostNetwork() {
			return ""
		}
		v := r.Container.Env[key]
		if secretRefInContent.MatchString(v) {
			return ""
		}
		return v
	}
	return ""
}

// LoopbackPlacementError refuses a consumer that would dial a URL its
// provider exported on loopback from somewhere that loopback is not the
// provider's.
//
// A provider on host networking may bind an unauthenticated API to
// 127.0.0.1 so that nothing off its machine can reach it (matter:server,
// #442). That URL means exactly one thing: the provider's node, seen from
// that node's own network namespace. A consumer on another node, or a
// consumer container on the Wildstone bridge (whose 127.0.0.1 is itself),
// renders fine, applies fine, and then never connects. Refusing at
// expansion is the one place the message can say why.
type LoopbackPlacementError struct {
	Package  string
	Provider string
	Key      string
	URL      string
	// Resource is "<Kind>/<name>" of the consumer's resource that carries
	// the URL, and Node where it would use it.
	Resource string
	Node     string
	// ProviderNodes are where the provider's host-networked containers
	// run. Unset when the refusal is about the bridge, not the node.
	ProviderNodes []string
	// Bridge is set when the reader is a container on the Wildstone
	// bridge rather than on the host's network.
	Bridge bool
}

func (e *LoopbackPlacementError) Error() string {
	if e.Bridge {
		return fmt.Sprintf("capability: package %s: %s uses %s (%s from %s), a loopback address on the host's network, "+
			"but it runs on the Wildstone bridge, where 127.0.0.1 is the container itself: give it networkMode: host",
			e.Package, e.Resource, e.Key, e.URL, e.Provider)
	}
	return fmt.Sprintf("capability: package %s: %s on node %q uses %s (%s from %s), which listens on loopback on node %s only. "+
		"Install %s on %s, or move %s",
		e.Package, e.Resource, e.Node, e.Key, e.URL, e.Provider, quoteList(e.ProviderNodes),
		e.Package, quoteList(e.ProviderNodes), e.Provider)
}

// loopbackURL reports whether v is a URL whose host is a loopback
// address. Only whole URLs are judged: a bare "127.0.0.1" also turns up
// in trust lists and health checks that have nothing to do with an
// export, and matching those would refuse consumers for no reason.
func loopbackURL(v string) bool {
	if !strings.Contains(v, "://") {
		return false
	}
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

// checkExportedLoopback holds a consumer to where a loopback URL its
// provider exported actually answers: a resource that carries the URL
// must be on a node where the provider runs a host-networked container,
// and if it is a container, it must be on the host's network itself.
//
// Generic, like checkExportedSecrets: the engine does not know what
// MatterWsURL is for, only that its value is a loopback URL and which of
// the consumer's resources carry it. A provider with nothing installed
// cannot be compared against and passes the node half; in a rehearsal
// the nodes are stand-ins and only the bridge half applies.
func checkExportedLoopback(out []resource.Resource, resolved map[string]string, exportedBy map[string]*pkgspec.Package, catalog *Catalog) error {
	if catalog == nil {
		return nil
	}
	keys := make([]string, 0, len(exportedBy))
	for k := range exportedBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := resolved[key]
		if !loopbackURL(v) {
			continue
		}
		provider := exportedBy[key]
		nodes := map[string]bool{}
		for _, r := range catalog.installed(provider) {
			if r.Kind == resource.KindContainer && r.Container.HostNetwork() {
				nodes[r.Metadata.Node] = true
			}
		}
		providerNodes := make([]string, 0, len(nodes))
		for n := range nodes {
			providerNodes = append(providerNodes, n)
		}
		sort.Strings(providerNodes)
		for _, r := range out {
			if !carriesValue(r, v) {
				continue
			}
			e := &LoopbackPlacementError{
				Package:  r.Metadata.Package,
				Provider: fmt.Sprintf("capability %s (package %s)", provider.Metadata.Capability, provider.Metadata.Name),
				Key:      key,
				URL:      v,
				Resource: string(r.Kind) + "/" + r.Metadata.Name,
				Node:     r.Metadata.Node,
			}
			// A Job always runs on the bridge (its spec cannot say host).
			if r.Kind == resource.KindJob || (r.Kind == resource.KindContainer && !r.Container.HostNetwork()) {
				e.Bridge = true
				return e
			}
			if catalog.rehearsal || len(nodes) == 0 || nodes[r.Metadata.Node] {
				continue
			}
			e.ProviderNodes = providerNodes
			return e
		}
	}
	return nil
}

// carriesValue reports whether a rendered resource hands v to something
// on its node: a container's (or a Job's) env value or argument, or the
// content of a managed or seeded file.
func carriesValue(r resource.Resource, v string) bool {
	has := func(ss ...string) bool {
		for _, s := range ss {
			if strings.Contains(s, v) {
				return true
			}
		}
		return false
	}
	envHas := func(env map[string]string) bool {
		for _, e := range env {
			if has(e) {
				return true
			}
		}
		return false
	}
	switch {
	case r.Kind == resource.KindContainer && r.Container != nil:
		return envHas(r.Container.Env) || has(r.Container.Command...)
	case r.Kind == resource.KindJob && r.Job != nil:
		return envHas(r.Job.Run.Env) || has(r.Job.Run.Command...)
	case r.Kind == resource.KindConfigFile && r.ConfigFile != nil:
		return has(r.ConfigFile.Content)
	case r.Kind == resource.KindSeedFile && r.SeedFile != nil:
		return has(r.SeedFile.Content)
	}
	return false
}
