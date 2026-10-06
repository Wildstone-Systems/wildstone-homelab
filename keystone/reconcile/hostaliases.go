package reconcile

// Which names a container must be able to RESOLVE, derived from the
// node's ingress posture rather than declared by any package.
//
// Under the tailnet posture every Route is served at the node's MagicDNS
// name, and the OIDC issuer is that same name because an issuer must
// match what the browser saw byte for byte (ADR-W019 A10). An app does
// not only send a browser there: it fetches the discovery document and
// exchanges the authorization code SERVER-SIDE, from inside its
// container — where Docker's embedded DNS has never heard of a .ts.net
// name. The result is "Name or service not known", a 500 on every login,
// and nothing in the plan, the events or the dashboard to explain it.
//
// It is derived, not declared, for the reason ADR-W029 A1 gives: a
// package cannot know which posture it will be installed under, and one
// that had to ask would be asking the operator for something Wildstone
// already knows.
//
// Applied to EVERY container rather than only the ones that look like
// OIDC consumers. An /etc/hosts line for a name the container never uses
// costs nothing, whereas guessing which containers need it means reading
// env vars and missing the app whose issuer is in a mounted config file
// — a detection heuristic that fails silently and exactly once per
// package.

import (
	"fmt"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// hostAliasesForNode returns the names containers on this node must
// resolve, and NOTHING if the node's agent is too old to write them.
//
// The version gate is not politeness, it is what stops a silent
// permanent failure. host_aliases arrived in protocol minor 14. A
// minor-13 agent ignores the field but still labels the container with
// whatever spec hash Keystone sent — so if the hash counted the aliases,
// a container created by the old agent would carry the ALIAS-AWARE hash
// while having no /etc/hosts entry at all. Upgrade the agent afterwards
// and observation finds a hash that matches, calls the container
// converged, and never recreates it: the entry never arrives, login
// stays broken, and every surface reports green.
//
// Returning nothing below minor 14 makes the hash identical to what it
// was before this feature existed, so nothing churns while the agent is
// old — and the moment the agent reports 14 the aliases enter the hash,
// the hash changes, and the container is recreated with its entry. The
// fix arrives by itself, in the right order, with no operator step.
//
// An UNKNOWN minor is treated as too old, for the same reason: assuming
// support we cannot see is how the silent version of this bug gets in.
func (r *Reconciler) hostAliasesForNode(nodeID string, desiredByKey map[resKey]resource.Resource) []string {
	if r.AgentProtocolMinor == nil {
		return nil
	}
	minor, known := r.AgentProtocolMinor(nodeID)
	if !known || minor < hostAliasesProtocolMinor {
		return nil
	}
	return hostAliasesFor(desiredByKey)
}

// hostAliasesProtocolMinor is the minor that introduced host_aliases.
const hostAliasesProtocolMinor = 14

// hostAliasesFor returns the names containers on this node must resolve.
//
// Empty under every posture but tailscale: with ordinary DNS the issuer
// resolves the same way inside a container as outside, and adding
// entries would only create a second answer that can go stale.
func hostAliasesFor(desiredByKey map[resKey]resource.Resource) []string {
	for _, r := range desiredByKey {
		if r.Kind != resource.KindIngress || r.Ingress == nil {
			continue
		}
		if r.Ingress.TLS == "tailscale" && r.Ingress.TailnetName != "" {
			return []string{r.Ingress.TailnetName}
		}
	}
	return nil
}

// containerHostAliases is nodeAliases plus, for the containers that dial
// Route upstreams, resource.HostGatewayAlias (#433).
//
// Per container rather than per node, unlike the tailnet name above: the
// alias is only useful to something that proxies or probes a
// "host/<port>" Route, and every container that carries it has its hash
// changed and is recreated once. Two kinds of container dial Route
// upstreams — the ingress proxy, which serves them, and the dashboard,
// whose status dot polls the same address (homepageMonitor) — so those
// two, and nothing else, get it.
//
// Only while a host Route exists on the node: a household with no
// host-networked service keeps its proxy's hash exactly as it was, and
// the first host Route recreates the proxy with the alias in the same
// plan that writes its fragment. Granting it grants nothing new either —
// a bridge container can already route to its gateway; the alias only
// gives that address a name, resolved by Docker on the node.
//
// Gated on the agent the same way hostAliasesForNode is, and for the same
// reason: an agent that cannot map the name to host-gateway would skip
// it while still labelling the container with the alias-aware hash, and
// the proxy would never be recreated once the agent could.
func (r *Reconciler) containerHostAliases(nodeID, name string, nodeAliases []string, desiredByKey map[resKey]resource.Resource) []string {
	if !r.agentMapsHostGateway(nodeID) || !dialsHostUpstreams(nodeID, name, desiredByKey) {
		return nodeAliases
	}
	out := make([]string, 0, len(nodeAliases)+1)
	out = append(out, nodeAliases...)
	return append(out, resource.HostGatewayAlias)
}

// refuseHostRouteIfAgentTooOld returns the step error for a "host/<port>"
// Route on a node whose agent is KNOWN to predate the host-gateway
// mapping, or "". Written anyway, the fragment would render and the
// proxy would answer 502 for it, because the proxy container never got
// the alias (containerHostAliases withholds it there). An unknown minor
// is not refused, matching refuseIfAgentTooOld: nothing can say.
func (r *Reconciler) refuseHostRouteIfAgentTooOld(nodeID string, step plan.PlanStep, spec *resource.RouteSpec) string {
	if spec == nil || !resource.IsHostUpstream(spec.Upstream) || r.AgentProtocolMinor == nil {
		return ""
	}
	have, known := r.AgentProtocolMinor(nodeID)
	if !known || have >= resource.HostGatewayProtocolMinor {
		return ""
	}
	return fmt.Sprintf("%s/%s routes to %s, which needs wardstone protocol minor %d, and the agent on node %s "+
		"speaks minor %d: upgrade wardstoned there first. An older agent cannot give the proxy a route to the "+
		"host, so the site would answer 502 with every resource reporting healthy",
		step.Kind, step.Name, spec.Upstream, resource.HostGatewayProtocolMinor, nodeID, have)
}

// agentMapsHostGateway reports whether the node's agent writes
// resource.HostGatewayAlias as Docker's host-gateway. Unknown is no, for
// the reason hostAliasesForNode gives.
func (r *Reconciler) agentMapsHostGateway(nodeID string) bool {
	if r.AgentProtocolMinor == nil {
		return false
	}
	minor, known := r.AgentProtocolMinor(nodeID)
	return known && minor >= resource.HostGatewayProtocolMinor
}

// dialsHostUpstreams reports whether Container/<name> is the node's
// ingress proxy or its dashboard, and the node has a "host/<port>" Route.
func dialsHostUpstreams(nodeID, name string, desiredByKey map[resKey]resource.Resource) bool {
	c, ok := desiredByKey[resKey{resource.KindContainer, name}]
	if !ok || c.Container == nil || c.Container.HostNetwork() {
		// A host-networked container is already on the host.
		return false
	}
	hostRoute, ingressOwner := false, ""
	for _, res := range desiredByKey {
		switch {
		case res.Kind == resource.KindRoute && res.Route != nil &&
			(res.Metadata.Node == "" || res.Metadata.Node == nodeID) &&
			resource.IsHostUpstream(res.Route.Upstream):
			hostRoute = true
		case res.Kind == resource.KindIngress:
			ingressOwner = res.Metadata.Owner
		}
	}
	if !hostRoute {
		return false
	}
	// The proxy is matched by owner, as ingressContainerNames does, so a
	// new ingress profile needs no entry here.
	if ingressOwner != "" && c.Metadata.Owner == ingressOwner {
		return true
	}
	return strings.TrimPrefix(c.Metadata.Owner, "capability/") == "dashboard"
}
