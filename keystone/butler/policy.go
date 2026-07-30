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
// spike's one rule, per ADR-W006 A3/S4b: a Route classified family_public
// may not have auth disabled. This is exactly the change the S4b
// injection-red-team test scripts an "obedient" model into requesting, and
// it must be rejected here regardless of the model's intent.
func policyValidate(res resource.Resource) error {
	if res.Kind != resource.KindRoute || res.Route == nil {
		return nil
	}
	if res.Route.Classification == publicClassification && res.Route.Auth == noAuth {
		return &PolicyError{Msg: fmt.Sprintf(
			"route %q is classified %s and may not have auth disabled (auth=%q)",
			res.Metadata.Name, publicClassification, noAuth,
		)}
	}
	return nil
}
