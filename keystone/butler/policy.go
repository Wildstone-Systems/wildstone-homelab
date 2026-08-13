package butler

import (
	"fmt"

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

// publicClassification is the Route classification that marks a route as
// exposed to the public internet (ADR-W004 A3's mandatory classification
// policy input).
const publicClassification = "family_public"

// noAuth is the RouteSpec.Auth value meaning "no authentication."
const noAuth = "none"

// policyValidate rejects proposed changes no principal — human or model —
// may make, regardless of how the request was phrased or what a plan's
// risk grade says (this is a hard reject, not a "needs approval"). The
// spike's one rule, per ADR-W006 A3/S4b: a Route that is public-facing may
// not have auth disabled.
//
// "Public-facing" is evaluated against BOTH the proposed classification and
// the current one for a route of the same name. Evaluating only the proposed
// resource let a model launder the rule in a single call — relabel a
// currently-family_public route as family_internal AND set auth=none, so the
// standalone proposed resource looks compliant while the live route keeps
// serving with auth off. Comparing against current state closes that
// downgrade path: you cannot drop auth on a route that is public now, no
// matter what classification the same call assigns it.
//
// current is the resource set the proposal is diffed against (the present
// Statefile); pass nil when there is no prior state to compare.
func policyValidate(res resource.Resource, current []resource.Resource) error {
	if res.Kind != resource.KindRoute || res.Route == nil {
		return nil
	}
	if res.Route.Auth != noAuth {
		return nil
	}
	proposedPublic := res.Route.Classification == publicClassification
	currentPublic := currentRouteIsPublic(current, res.Metadata.Name)
	if proposedPublic || currentPublic {
		reason := "is classified " + publicClassification
		if !proposedPublic && currentPublic {
			reason = "is currently classified " + publicClassification +
				" (a classification downgrade in the same change does not lift the rule)"
		}
		return &PolicyError{Msg: fmt.Sprintf(
			"route %q %s and may not have auth disabled (auth=%q)",
			res.Metadata.Name, reason, noAuth,
		)}
	}
	return nil
}

// currentRouteIsPublic reports whether a Route named name already exists in
// current and is classified family_public.
func currentRouteIsPublic(current []resource.Resource, name string) bool {
	for _, r := range current {
		if r.Kind == resource.KindRoute && r.Metadata.Name == name && r.Route != nil {
			return r.Route.Classification == publicClassification
		}
	}
	return false
}
