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
