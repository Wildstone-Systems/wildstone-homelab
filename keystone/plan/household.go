package plan

// Household-routine plan classification (ADR-W030 A8/A8.1, work
// package P13).
//
// A plan that only changes what the house DOES — its routines — is a
// different sort of decision from one that changes what the house IS.
// An admitted adult should be able to review the first without ever
// holding Console credentials; nobody should be able to review the
// second that way.
//
// So the classification is a property of the plan's steps, computed
// rather than asserted, and it is deliberately strict: a plan is a
// household-routine plan only if EVERY step is an Automation. One
// infrastructure step, one identity step, one anything-else, and the
// whole plan goes back to the Console — because "mostly routines" is
// exactly the shape an escalation would take.

import "github.com/chickenrat17/wildstone-homelab/keystone/resource"

// Classification of a plan for review purposes.
const (
	// ClassHouseholdRoutine is a plan whose every step is an Automation.
	// An admitted adult may review it through the household page.
	ClassHouseholdRoutine = "household-routine"
	// ClassInfrastructure is everything else: containers, routes,
	// volumes, identity, secrets. Console only.
	ClassInfrastructure = "infrastructure"
	// ClassMixed is a plan that touches both. It reviews as
	// infrastructure — the safe direction — and is named separately so
	// a refusal can say WHY rather than just no.
	ClassMixed = "mixed"
	// ClassEmpty is a plan with no steps.
	ClassEmpty = "empty"
)

// Classify reports how a plan may be reviewed.
//
// The rule is all-or-nothing on purpose. A plan that adds a routine and
// also opens a route is not a household decision with an infrastructure
// footnote; it is an infrastructure change, and the routine in it is
// what makes it look approachable.
func Classify(p *Plan) string {
	if p == nil || len(p.Steps) == 0 {
		return ClassEmpty
	}
	routines, other := 0, 0
	for _, s := range p.Steps {
		if s.Kind == resource.KindAutomation {
			routines++
			continue
		}
		other++
	}
	switch {
	case other == 0:
		return ClassHouseholdRoutine
	case routines == 0:
		return ClassInfrastructure
	default:
		return ClassMixed
	}
}

// ReviewableByHousehold reports whether an admitted adult may approve
// this plan through the household review page.
//
// It is a function rather than a field so it cannot be set: a stored
// boolean saying "this is safe for the household" is a boolean somebody
// eventually sets wrongly, and the steps are right there.
func ReviewableByHousehold(p *Plan) bool {
	return Classify(p) == ClassHouseholdRoutine
}
