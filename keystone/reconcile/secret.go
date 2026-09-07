package reconcile

// Secret applier: a secret that must exist on the node, generated there.
//
// Keystone never sees the material. EnsureSecret creates it on the node
// if absent and reports only whether it existed and at what version, so
// a Secret's whole lifecycle here is "ask, and be told yes" (ADR-W012).
//
// ORDERING IS THE HARD PART, and kindOrder does not solve it on its own.
// Wildstone writes managed files BEFORE any plan step runs — the base
// Caddyfile, the merged Authelia clients file — and those can carry
// secret references the node resolves at write time. A secret created by
// a plan step would therefore be created too late: the prereq write
// fails with NOT_FOUND, and a failed prereq aborts the whole tick before
// the plan that would have created the secret ever runs. That is a
// deadlock, not a slow convergence, and no amount of retrying escapes
// it.
//
// So desired secrets are ensured in a pre-pass, before prereq repair.
// This is the same category as the docker network in ensureNodePrereqs:
// a thing that must exist for Wildstone's own files and containers to be
// writable at all. Like the prereq files, it runs outside the plan and
// therefore outside approval — defensible on exactly the grounds the
// prereq files are, since creating a secret is additive, affects nothing
// that exists, and is what a package asked for by declaring it.
//
// The plan step still exists and still calls EnsureSecret. EnsureSecret
// is idempotent, so on the tick where the pre-pass created it the step
// reports existed=true; that is not a redundancy to remove. The pre-pass
// answers "can the files be written", and the step answers "is this
// resource applied" — the operator's plan should say a secret was
// created, and the managed cache should know about it.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// secretKindWire maps SecretSpec.Kind onto the wire enum. Empty is
// RANDOM, matching both the spec's documented default and the enum's
// UNSPECIFIED semantics, so an older agent asked for a random secret by
// a newer Keystone still gets what was meant.
func secretKindWire(kind string) wsv1.SecretKind {
	switch kind {
	case "rsa_private_key":
		return wsv1.SecretKind_SECRET_KIND_RSA_PRIVATE_KEY
	default:
		return wsv1.SecretKind_SECRET_KIND_RANDOM
	}
}

func ensureSecretOp(spec *resource.SecretSpec) *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureSecret{EnsureSecret: &wsv1.EnsureSecret{
		Name:        spec.Name,
		Kind:        secretKindWire(spec.Kind),
		LengthBytes: uint32(spec.LengthBytes),
	}}}
}

// ensureNodeSecrets creates every desired Secret before anything that
// could reference one is written. Returns "" on success, or the error
// text, which the caller treats exactly as a failed prereq.
//
// Idempotent by construction: EnsureSecret without rotate is
// create-if-absent, so this runs on every tick and does nothing after
// the first.
func listSecretsOp() *wsv1.Operation {
	return &wsv1.Operation{Op: &wsv1.Operation_ListSecrets{ListSecrets: &wsv1.ListSecrets{}}}
}

// awaitingProvidedSecrets reports which CAPABILITIES cannot converge for
// want of a secret only the operator can supply, keyed by owner, plus a
// hard error if the node could not be asked at all.
//
// It used to abort the whole node (ADR-W028). One capability enabled
// without its secrets stopped every other capability on the machine from
// converging, so an unrelated install approved afterwards failed too,
// with a message about secrets it had never heard of. The scope of the
// problem is the capability that declared the secret; the scope of the
// consequence should match it.
func (r *Reconciler) awaitingProvidedSecrets(
	ctx context.Context, nodeID string, desired []resource.Resource,
) (map[string]string, string) {
	byOwner := map[string][]*resource.SecretSpec{}
	var all []*resource.SecretSpec
	for _, res := range desired {
		if res.Kind != resource.KindSecret || res.Secret == nil || !res.Secret.Provided {
			continue
		}
		all = append(all, res.Secret)
		owner := res.Metadata.Owner
		byOwner[owner] = append(byOwner[owner], res.Secret)
	}
	if len(all) == 0 {
		return nil, ""
	}
	have, err := r.presentSecrets(ctx, nodeID)
	if err != "" {
		// Not being able to ASK is a node-wide fault, and stays one:
		// converging on a guess about which secrets exist is worse than
		// not converging.
		return nil, err
	}
	blocked := map[string]string{}
	for owner, specs := range byOwner {
		var missing []string
		for _, spec := range specs {
			if !have[spec.Name] {
				missing = append(missing, spec.Name)
			}
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		var b strings.Builder
		// All of this owner's missing secrets, not the first: the whole
		// complaint behind F37 is that they surface one per tick, so an
		// operator fixes one and meets the next.
		fmt.Fprintf(&b, "%d secret(s) must be supplied before %s can converge. They are declared, "+
			"not generated, because only you can produce a correct value:",
			len(missing), strings.TrimPrefix(owner, "capability/"))
		for _, n := range missing {
			fmt.Fprintf(&b, "\n    wild secret set %s -node %s", n, nodeID)
		}
		blocked[owner] = b.String()
	}
	return blocked, ""
}

// presentSecrets is what the node says it holds.
func (r *Reconciler) presentSecrets(ctx context.Context, nodeID string) (map[string]bool, string) {
	out, err := r.retryDispatch(ctx, nodeID, listSecretsOp())
	if err != nil {
		return nil, fmt.Sprintf("listing secrets to check the ones you must supply: %v", err)
	}
	if oe := out.GetError(); oe != nil {
		return nil, fmt.Sprintf("listing secrets to check the ones you must supply: %s: %s",
			oe.GetCode(), oe.GetMessage())
	}
	have := make(map[string]bool)
	for _, si := range out.GetListSecrets().GetSecrets() {
		have[si.GetName()] = true
	}
	return have, ""
}

// ensureNodeSecrets generates every desired Secret that is not
// operator-supplied. Capabilities awaiting a supplied secret are skipped
// rather than aborting the node: their steps are removed from the plan
// by the caller, so generating their other secrets would be work for a
// capability that is not going to converge this pass.
func (r *Reconciler) ensureNodeSecrets(
	ctx context.Context, nodeID string, desired []resource.Resource, awaiting map[string]string,
) string {
	if nodeID == "" {
		return ""
	}
	for _, res := range desired {
		if _, blocked := awaiting[res.Metadata.Owner]; blocked {
			continue
		}
		if res.Kind != resource.KindSecret || res.Secret == nil {
			continue
		}
		if res.Secret.Provided {
			continue // never generated; presence was checked above
		}
		out, err := r.retryDispatch(ctx, nodeID, ensureSecretOp(res.Secret))
		if err != nil {
			return fmt.Sprintf("ensure secret/%s: %v", res.Secret.Name, err)
		}
		if oe := out.GetError(); oe != nil {
			return fmt.Sprintf("ensure secret/%s: %s: %s", res.Secret.Name, oe.GetCode(), oe.GetMessage())
		}
	}
	return ""
}

// observeSecret reports a Secret present iff a prior successful apply
// recorded it, the declared-state pattern Volume uses.
//
// It deliberately does NOT ask the node whether the secret exists, even
// though ListSecrets could. A secret's existence is not the whole of its
// desired state — its KIND is too, and nothing on the wire reports the
// kind of an existing secret. Reporting "present" from a name alone
// would call an RSA key present when a random blob is what is there,
// which is the failure this kind was added to prevent. Declared state
// is the honest answer until the agent can say what a secret is.
func (r *Reconciler) observeSecret(nodeID string, k resKey, want resource.Resource, isDesired bool) (resource.Resource, bool) {
	if !r.isManaged(nodeID, k) {
		return resource.Resource{}, false
	}
	if isDesired {
		return want, true
	}
	return placeholderResource(resource.KindSecret, k.Name), true
}

// applySecretStep ensures the secret and records it.
//
// A Delete records the declaration going away and does nothing on the
// node: no op removes a secret. That is deliberate rather than missing —
// a control plane that can delete key material on a state change is one
// bad plan away from destroying the only copy of something. Removal is a
// human's job, with the runbook open.
func (r *Reconciler) applySecretStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.Secret == nil {
			sr.Error = "internal: no desired Secret spec for " + step.Name
			return sr
		}
		res, err := r.retryDispatch(ctx, nodeID, ensureSecretOp(want.Secret))
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		if oe := res.GetError(); oe != nil {
			sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
			return sr
		}
		sr.Applied = true
		// The health gate is the op's own answer: the agent reports a
		// version, and a secret with a version exists. There is nothing
		// further to probe, and probing the VALUE is exactly what this
		// subsystem exists to make impossible.
		sr.HealthOK = res.GetEnsureSecret().GetVersion() > 0
		if !sr.HealthOK {
			sr.Error = fmt.Sprintf("secret/%s reported version 0 after EnsureSecret", want.Secret.Name)
			return sr
		}
		r.markManaged(nodeID, key, true)
		return sr

	case plan.ActionDelete:
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}
