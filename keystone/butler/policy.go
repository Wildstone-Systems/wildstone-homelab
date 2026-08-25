package butler

import (
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/policy"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// PolicyError is returned by policyValidate when a proposed change is
// rejected outright, independent of any human approval step (ADR-W006 A3:
// "the Butler cannot request policy-invalid changes... policy rejects them
// regardless of principal").
type PolicyError struct {
	Msg string
}

func (e *PolicyError) Error() string { return "policy: " + e.Msg }

// policyValidate rejects proposed changes no principal — human or model —
// may make, regardless of how the request was phrased or what a plan's
// risk grade says (a hard reject, not a "needs approval"). Since E4
// (ADR-W009) it delegates to the shared keystone/policy engine — the same
// rules every write path enforces — evaluated as role "butler", which
// additionally arms the butler-admin-scope rule. The S4 spike's single
// rule (family_public routes keep auth, including the downgrade-laundering
// guard) lives on as the engine's public-requires-auth rule; the S4
// red-team property is unchanged: a fully-obeyed injection still cannot
// produce a policy-valid proposal for a forbidden change.
//
// current is the resource set the proposal is diffed against (the present
// Statefile); res is the single resource the change proposes.
func policyValidate(res resource.Resource, current []resource.Resource) error {
	// Build the target tree the change would produce: current with res
	// replaced (by kind+name) or appended.
	proposed := make([]resource.Resource, 0, len(current)+1)
	for _, c := range current {
		if c.Kind == res.Kind && c.Metadata.Name == res.Metadata.Name {
			continue
		}
		proposed = append(proposed, c)
	}
	proposed = append(proposed, res)

	// Exemptions are a Keystone-host configuration (ADR-W009 A3); the
	// in-process spike Butler runs with none — the strictest posture.
	violations, _ := policy.NewEngine(nil).Validate(policy.Change{
		Proposed:  proposed,
		Current:   current,
		Principal: "butler",
		Role:      "butler",
	})
	if len(violations) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(violations))
	for _, v := range violations {
		msgs = append(msgs, v.String())
	}
	return &PolicyError{Msg: strings.Join(msgs, "; ")}
}
