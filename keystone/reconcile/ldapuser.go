package reconcile

// LdapUser applier: declaration only, exactly like LdapGroup.
//
// Apply makes no external call. It records the person in the
// Reconciler's managed cache; what the declaration FEEDS is the rendered
// user list (lldapusers.go), written as a node prereq and read by the
// identity capability's provisioning Job, which creates any user the
// directory does not have and adds any membership it is missing.
//
// The split is the same one LdapGroup draws: this file decides the person
// is DESIRED, the prereq render decides what the node is TOLD, and the
// Job is what reaches LLDAP. Keystone has no route to a container on the
// node's Docker network, which is why provisioning is a Job at all.
//
// Delete records the declaration going away and does nothing on the node.
// No op removes a person, and that is a choice: an account carries a
// household's photos and documents, and "the plan said delete" must not
// be read as "the person is gone" (ADR-W020 A3).

import (
	"context"
	"log"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// applyLdapUserStep records step's LdapUser as managed (Create/Update) or
// unmanaged (Delete) and always reports success. nodeID keys the
// managed-cache lookup consistently with the other kinds; LdapUser is not
// truly node-scoped.
func (r *Reconciler) applyLdapUserStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	_ = ctx // no external call is made; ctx is accepted for signature symmetry
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.LdapUser == nil {
			sr.Error = "internal: no desired LdapUser spec for " + step.Name
			return sr
		}
		log.Printf("reconcile: LdapUser %q declared; the rendered user list and the identity "+
			"capability's provisioning job are what create it", want.LdapUser.Username)
		sr.Applied = true
		sr.HealthOK = true // trivially passes: declared/best-effort (ADR-W005 A3)
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		log.Printf("reconcile: LdapUser %q removed from desired state; it drops out of the rendered "+
			"user list. The account itself is NOT deleted", step.Name)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr
	}
	sr.Error = "unexpected action " + step.Action.String()
	return sr
}
