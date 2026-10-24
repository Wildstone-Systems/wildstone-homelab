package reconcile

import (
	"sort"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// DependentRestarts lists the containers and jobs a change will recreate
// or re-run although their own Statefile content does not change.
//
// A container's identity on the node is its spec hash, and that hash
// covers more than its own resource: the managed files it mounts
// (configdigest.go), the names it must resolve (hostaliases.go) and the
// network it joins. A Route or OidcClient added by one package therefore
// restarts the proxy and the identity provider, whose resources nobody
// edited. Bounded approval (ADR-W008 A2a) covers only reviewed steps, so
// those restarts must be part of what is reviewed: proposals add the
// steps returned here to the Plan the Owner approves.
//
// The prediction uses the same functions observe and apply use, per
// node, as the reconciler groups resources. It assumes the agent supports
// every alias it could be given; an older agent gets fewer, which can
// only make a predicted restart unnecessary (an entry nothing uses), never
// miss one. A restart it still misses is held and proposed as drift
// rather than run unreviewed.
func DependentRestarts(managedPrefix string, before, after []resource.Resource) []plan.PlanStep {
	prior := make(map[resKey]resource.Resource, len(before))
	for _, r := range before {
		prior[resKey{r.Kind, r.Metadata.Name}] = r
	}
	beforeByNode, afterByNode := keyedByNode(before), keyedByNode(after)

	var steps []plan.PlanStep
	for _, r := range after {
		if r.Kind != resource.KindContainer && r.Kind != resource.KindJob {
			continue
		}
		old, ok := prior[resKey{r.Kind, r.Metadata.Name}]
		if !ok || old.Metadata.Node != r.Metadata.Node || plan.Digest(old) != plan.Digest(r) {
			continue // created or edited: plan.Diff already has its step
		}
		node := r.Metadata.Node
		if effectiveHash(managedPrefix, node, r, beforeByNode[node]) == effectiveHash(managedPrefix, node, r, afterByNode[node]) {
			continue
		}
		what := "container"
		if r.Kind == resource.KindJob {
			what = "provisioning job"
		}
		steps = append(steps, plan.PlanStep{
			Description: "Restart " + string(r.Kind) + "/" + r.Metadata.Name + ": its configuration changes with this change",
			Kind:        r.Kind,
			Name:        r.Metadata.Name,
			Action:      plan.ActionUpdate,
			Risk:        plan.RiskMedium,
			BlastRadius: what + " recreated with its new configuration: brief interruption, no data affected",
			Digest:      plan.Digest(r),
		})
	}
	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Kind != steps[j].Kind {
			return steps[i].Kind < steps[j].Kind
		}
		return steps[i].Name < steps[j].Name
	})
	return steps
}

func keyedByNode(rs []resource.Resource) map[string]map[resKey]resource.Resource {
	out := map[string]map[resKey]resource.Resource{}
	for _, r := range rs {
		node := r.Metadata.Node
		if out[node] == nil {
			out[node] = map[resKey]resource.Resource{}
		}
		out[node][resKey{r.Kind, r.Metadata.Name}] = r
	}
	return out
}

// effectiveHash is the spec hash the reconciler would stamp on r given the
// rest of its node's desired state, as observeContainer and observeJob
// compute it.
func effectiveHash(managedPrefix, node string, r resource.Resource, nodeDesired map[resKey]resource.Resource) string {
	cfg := managedConfigContents(managedPrefix, nodeDesired)
	aliases := hostAliasesFor(nodeDesired)
	switch r.Kind {
	case resource.KindJob:
		spec := jobRunSpec(r.Job)
		return specHash(spec, ContainerNetwork, configDigest(managedPrefix, spec, cfg), aliases)
	default:
		if dialsHostUpstreams(node, r.Metadata.Name, nodeDesired) {
			aliases = append(append([]string{}, aliases...), resource.HostGatewayAlias)
		}
		return specHash(r.Container, containerNetworkFor(r.Container), configDigest(managedPrefix, r.Container, cfg), aliases)
	}
}
