package capability

// The package-managed start path for an integration adapter (ADR-W030
// A5/A5.1, epic #326 S01).
//
// Expand renders what INSTALLING a package produces, and it never looks
// at `integrates:`. That is the whole of the dormancy guarantee: a
// package that bundles an adapter installs exactly the resources its
// templates declare, and no adapter container, credential or connection
// among them.
//
// ExpandAdapter is the other half. It renders the adapter for ONE
// connection on ONE prepared node — what an approving placement Plan
// deploys before that node becomes eligible (A5.1: "the approving
// placement Plan deploys and starts the adapter container and provisions
// required connection secrets on every listed node"). Stopping is the
// same resources removed, which is what a placement removal proves.
//
// The adapter inherits the installed package's own params by name
// (`inherits:`), its package version and the node it is placed on. It
// never inherits a credential: those arrive as node secret references in
// its scoped environment, named by the placement, and their values never
// pass through Keystone, a param, or this rendering.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The environment an adapter is rendered with. Part of adapter contract
// 1 (pkgspec.AdapterContract); the program reads these and nothing else.
const (
	AdapterEnvKeystoneAPI    = "KEYSTONE_API"
	AdapterEnvKeystoneToken  = "KEYSTONE_TOKEN"
	AdapterEnvConnection     = "WILD_CONNECTION"
	AdapterEnvNode           = "WILD_NODE"
	AdapterEnvPackage        = "WILD_PACKAGE"
	AdapterEnvPackageVersion = "WILD_PACKAGE_VERSION"
	AdapterEnvContract       = "WILD_ADAPTER_CONTRACT"
	// AdapterEnvParamPrefix + UPPER(param) carries each inherited param.
	AdapterEnvParamPrefix = "WILD_PARAM_"
	// AdapterEnvSecretPrefix + UPPER(need) carries each credential need,
	// always as a "secret/<name>" node reference.
	AdapterEnvSecretPrefix = "WILD_SECRET_"
)

// AdapterPlacement is one prepared node for one connection.
type AdapterPlacement struct {
	// Connection is the integration connection's name.
	Connection string
	// Node is the placement node. Chosen by the Plan, never by the
	// package.
	Node string
	// KeystoneURL is the API as the adapter's node reaches it.
	KeystoneURL string
	// TokenSecret is the node secret holding this placement's adapter
	// principal token (minted by Keystone, provided on the node).
	TokenSecret string
	// Secrets maps a declared credential need to the node secret that
	// holds it for THIS connection. A need with no entry is left out, and
	// the adapter reports itself not configured rather than guessing.
	Secrets map[string]string
	// Env is the connection's own non-secret environment — for mail, the
	// Person's login and their authorized source filters
	// (runtimedb.AdapterPlacementEnv). Keys must begin WILD_ and may not
	// shadow the contract's own names or prefixes; a value may not be a
	// secret reference, because credentials travel only through Secrets.
	Env map[string]string
}

var adapterConnectionRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// AdapterOwner is the ownership chain of a rendered adapter. Not the
// capability's: an upgrade diffs "resources this capability has that the
// new rendering does not produce", and an adapter is never produced by
// Expand, so owning it by the capability would read a running adapter
// as upgrade debris and propose deleting it.
func AdapterOwner(connection string) string { return "integration/" + connection }

// AdapterContainerName names the adapter container for one connection
// and domain on one placement node. The lease, not the name, says which
// one is active; the node is in the name because desired state keys a
// resource by kind and name alone, so one container per prepared node
// needs one name per node.
func AdapterContainerName(domain, connection, node string) string {
	return domain + "-adapter-" + connection + "-" + node
}

// AdapterPrincipalName names the adapter principal (and, with
// AdapterTokenSecretName, the node secret holding its token) for one
// connection on one node — node-bound, so it claims only while that node
// holds the connection's lease (ADR-W030 A5.2).
func AdapterPrincipalName(domain, connection, node string) string {
	return domain + "-adapter-" + connection + "-" + node
}

// AdapterTokenSecretName is the node secret an adapter principal's token
// is placed in.
func AdapterTokenSecretName(domain, connection, node string) string {
	return AdapterPrincipalName(domain, connection, node) + "-token"
}

// adapterSecretResourceName keys one node's declaration of a connection
// secret. The node secret keeps its own name (SecretSpec.Name, the
// secret/<name> target); only the desired-state key is per node.
func adapterSecretResourceName(ref, node string) string {
	return ref + "." + node
}

// ExpandAdapter renders the adapter a package declares for domain, for
// one connection placed on one node. params are the INSTALLED package's
// params, exactly as the install recorded them.
func (e *Engine) ExpandAdapter(pkg *pkgspec.Package, params map[string]string, domain string, pl AdapterPlacement) ([]resource.Resource, error) {
	in, ok := pkg.AdapterFor(domain)
	if !ok {
		return nil, fmt.Errorf("capability: package %s declares no %s adapter", pkg.Metadata.Name, domain)
	}
	if !adapterConnectionRegexp.MatchString(pl.Connection) {
		return nil, fmt.Errorf("capability: connection %q must be lowercase letters, digits and inner dashes", pl.Connection)
	}
	if pl.Node == "" || pl.KeystoneURL == "" || pl.TokenSecret == "" {
		return nil, fmt.Errorf("capability: an adapter placement needs a node, the Keystone URL and its token secret")
	}
	merged, err := mergeParams(pkg, params)
	if err != nil {
		return nil, err
	}
	a := in.Adapter
	name := AdapterContainerName(domain, pl.Connection, pl.Node)
	owner := AdapterOwner(pl.Connection)
	meta := func(n string) resource.Metadata {
		return resource.Metadata{Name: n, Owner: owner, Package: pkg.Metadata.Name, Node: pl.Node}
	}

	env := map[string]string{
		AdapterEnvKeystoneAPI:    pl.KeystoneURL,
		AdapterEnvKeystoneToken:  "secret/" + pl.TokenSecret,
		AdapterEnvConnection:     pl.Connection,
		AdapterEnvNode:           pl.Node,
		AdapterEnvPackage:        pkg.Metadata.Name,
		AdapterEnvPackageVersion: pkg.Metadata.Version,
		AdapterEnvContract:       fmt.Sprint(a.Contract),
	}
	for _, p := range a.Inherits {
		env[AdapterEnvParamPrefix+envName(p)] = merged[p]
	}
	for k, v := range pl.Env {
		if err := checkPlacementEnv(k, v); err != nil {
			return nil, err
		}
		if _, taken := env[k]; taken {
			return nil, fmt.Errorf("capability: placement env %s shadows the adapter contract's own", k)
		}
		env[k] = v
	}
	out := []resource.Resource{{
		ApiVersion: resource.ApiVersion, Kind: resource.KindSecret, Metadata: meta(pl.TokenSecret),
		Secret: &resource.SecretSpec{Name: pl.TokenSecret, Provided: true,
			Purpose: "This adapter's Keystone token: claim and report work for its own connection, nothing else."},
	}}
	needs := append([]pkgspec.AdapterSecretNeed(nil), a.Needs.Secrets...)
	sort.Slice(needs, func(i, j int) bool { return needs[i].Name < needs[j].Name })
	for _, need := range needs {
		ref, ok := pl.Secrets[need.Name]
		if !ok || ref == "" {
			continue
		}
		env[AdapterEnvSecretPrefix+envName(need.Name)] = "secret/" + ref
		out = append(out, resource.Resource{
			ApiVersion: resource.ApiVersion, Kind: resource.KindSecret, Metadata: meta(adapterSecretResourceName(ref, pl.Node)),
			Secret: &resource.SecretSpec{Name: ref, Provided: true, Purpose: need.Purpose},
		})
	}
	mounts := []resource.Mount{{Source: pkgspec.AdapterProgramDir, Target: pkgspec.AdapterProgramMount, ReadOnly: true}}
	for _, m := range a.Needs.Mounts {
		mounts = append(mounts, resource.Mount{Source: m.Source, Target: m.Target, ReadOnly: true})
	}
	container := resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindContainer, Metadata: meta(name),
		Container: &resource.ContainerSpec{
			Image:   a.Image,
			Command: []string{pkgspec.AdapterProgramMount + "/" + a.Program},
			Env:     env,
			Mounts:  mounts,
			Restart: "unless-stopped",
			// Contained on purpose, and load-bearing twice over. An adapter
			// needs to dial out and nothing else. And these fields need
			// wardstone protocol minor 18, so the reconciler refuses to
			// hand this container to an older agent at all — the Wardstone
			// half of failing closed on skew.
			ReadOnlyRootfs:  true,
			NoNewPrivileges: true,
			CapDrop:         []string{"ALL"},
			Resources:       &resource.ContainerResources{Memory: "256m", PidsLimit: 64},
		},
	}
	out = append(out, container)
	for i := range out {
		if err := out[i].Validate(); err != nil {
			return nil, fmt.Errorf("capability: rendering %s adapter for %s: %w", domain, pl.Connection, err)
		}
	}
	return out, nil
}

func envName(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(s))
}

var placementEnvKey = regexp.MustCompile(`^WILD_[A-Z0-9_]{1,60}$`)

// checkPlacementEnv refuses a per-connection env entry that could pass
// for part of the contract or carry a credential.
func checkPlacementEnv(k, v string) error {
	if !placementEnvKey.MatchString(k) || strings.HasPrefix(k, AdapterEnvParamPrefix) ||
		strings.HasPrefix(k, AdapterEnvSecretPrefix) {
		return fmt.Errorf("capability: placement env key %q must be WILD_<NAME> outside the param and secret prefixes", k)
	}
	if strings.HasPrefix(v, "secret/") {
		return fmt.Errorf("capability: placement env %s may not reference a secret; credentials go through Secrets", k)
	}
	if len(v) > 16<<10 {
		return fmt.Errorf("capability: placement env %s is too large", k)
	}
	return nil
}
