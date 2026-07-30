package reconcile

// LdapGroup applier (ADR-W005 A3/S3a): declared + best-effort. Per the
// handoff trap ADR-W005 quotes ("per-app identity attribution will be
// messy — packages declare best-achievable"), S3 does NOT build an LLDAP
// API client. Apply is a documented log-and-record no-op: it never talks
// to any external system, always "succeeds", and its only effect is
// recording the group in the Reconciler's in-process managed cache (the
// same mechanism Volume's observe-by-apply already uses — see
// markManaged/isManaged in reconcile.go). Real LLDAP provisioning is a
// later provider, analogous to how DnsRecord's real provider (Cloudflare)
// was added in S2c after S2b modeled DnsRecord more thinly.
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

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
		log.Printf("reconcile: LdapGroup %q declared (best-effort, no LLDAP call made): members=%v", want.LdapGroup.Name, want.LdapGroup.Members)
		sr.Applied = true
		sr.HealthOK = true // trivially passes: declared/best-effort (ADR-W005 A3)
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		log.Printf("reconcile: LdapGroup %q removed from desired state (declared only; no external delete performed)", step.Name)
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}
