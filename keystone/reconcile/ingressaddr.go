package reconcile

// Where the identity proxy speaks from, kept current.
//
// A forwarded Remote-User is honoured only from the exact peer the
// proxy is believed to be (ADR-W030 A8.1), and that belief used to be a
// flag set once at install time. The flag is a snapshot of a Docker
// container's address, and Docker is free to hand a recreated container
// a different one — which this very reconciler does, every time an
// ingress spec changes.
//
// A stale value does not degrade gracefully. Forward-auth refuses
// everyone, the household portal refuses everyone, and (since the pages
// gate) the Console refuses to render at all, so the household loses
// its control plane with the proxy still insisting people are signed
// in. Nothing reports it, because every component involved is behaving
// exactly as designed.
//
// So it is observed rather than configured. Read on every tick, before
// any plan is built, because it must stay right whether or not there is
// anything to apply — and because the tick that recreates the proxy is
// precisely the tick after which the old address is wrong.

import (
	"context"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// ingressContainerNames returns the Containers belonging to the same
// capability as the node's Ingress resource — the proxy itself.
//
// Matched by OWNER rather than by name. Four packages provide the
// ingress capability and each calls its container something different
// (caddy-public, caddy-tailnet, caddy-tailnet-public, caddy-tailnet-ts),
// so a name list here would be a fifth place to update when a profile is
// added, and the failure of forgetting would be silent: the address
// would simply never refresh.
//
// Returns nothing when the node has no Ingress resource, which is the
// ordinary state of a household with no proxy and must stay
// indistinguishable from it.
func ingressContainerNames(desired []resource.Resource) []string {
	owner := ""
	for _, res := range desired {
		if res.Kind == resource.KindIngress {
			owner = res.Metadata.Owner
			break
		}
	}
	if owner == "" {
		return nil
	}
	var out []string
	for _, res := range desired {
		if res.Kind == resource.KindContainer && res.Metadata.Owner == owner {
			out = append(out, res.Metadata.Name)
		}
	}
	return out
}

// refreshIngressAddrs inspects the proxy's containers and reports their
// addresses through the NoteIngressAddrs seam.
//
// Silent on every failure, and that is the deliberate choice rather than
// laziness. The inspect fails for reasons that are all normal at some
// point in a node's life — the container has not been created yet, the
// node is not connected, the agent predates the field — and in every one
// of them the right answer is to leave the previous belief alone. An
// empty result means "unknown", never "the proxy has no address": acting
// on it would revoke a working trust boundary the moment a node
// reconnected slowly.
//
// The seam is only called when there is something to say, so a household
// with no ingress never has its configured value overwritten by nothing.
func (r *Reconciler) refreshIngressAddrs(ctx context.Context, nodeID string, desired []resource.Resource) {
	if r.NoteIngressAddrs == nil {
		return
	}
	names := ingressContainerNames(desired)
	if len(names) == 0 {
		return
	}
	var addrs []string
	for _, name := range names {
		res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{
			Op: &wsv1.Operation_InspectContainer{
				InspectContainer: &wsv1.InspectContainer{Name: name},
			},
		})
		if err != nil {
			continue
		}
		addrs = append(addrs, res.GetInspectContainer().GetIpAddresses()...)
	}
	if len(addrs) == 0 {
		return
	}
	r.NoteIngressAddrs(nodeID, addrs)
}
