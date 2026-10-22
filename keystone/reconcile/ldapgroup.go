package reconcile

// LdapGroup applier: declaration only, and now with something reading
// the declaration.
//
// Apply still makes no external call — it records the group in the
// Reconciler's managed cache and nothing else, the same shape Volume's
// observe-by-apply uses. What changed is what the declaration FEEDS: every
// LdapGroup in the deployment is merged into the rendered group list
// (lldapgroups.go), written as a node prereq, and read by the identity
// capability's provisioning Job, which creates any group the directory
// does not have.
//
// So the split is: this file decides the group is DESIRED, the prereq
// render decides what the node is TOLD, and the Job is what reaches
// LLDAP. Keeping the API call out of here is deliberate — Keystone has
// no route to a container on the node's Docker network, which is the
// same reason DnsRecord needed a Provider and provisioning needed a Job.
//
// Observe: "declared-state from that cache" — reconcile.go's observe()
// treats an LdapGroup exactly like a Volume: present iff a prior
// successful apply recorded it as managed for this Reconciler instance.
//
// Health gate: trivially passes (ADR-W005 A3) — there is no live signal to
// check, since nothing external was actually changed.

import (
	"context"
	"log"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// applyLdapGroupStep records step's LdapGroup as managed (Create/Update) or
// unmanaged (Delete) and always reports success — there is no Wardstone op
// or external LLDAP call involved at all. nodeID is accepted only to key
// the managed-cache lookup consistently with the other kinds' apply
// functions; LdapGroup is not actually node-scoped.
func (r *Reconciler) applyLdapGroupStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	_ = ctx // no external call is made; ctx is accepted for signature symmetry with the other apply* methods
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.LdapGroup == nil {
			sr.Error = "internal: no desired LdapGroup spec for " + step.Name
			return sr
		}
		log.Printf("reconcile: LdapGroup %q declared; the rendered group list and the identity "+
			"capability's provisioning job are what create it", want.LdapGroup.Name)
		sr.Applied = true
		sr.HealthOK = true // trivially passes: declared/best-effort (ADR-W005 A3)
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		log.Printf("reconcile: LdapGroup %q removed from desired state; it drops out of the rendered "+
			"group list, and nothing deletes it from the directory — removing a group would remove "+
			"everyone in it", step.Name)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}
