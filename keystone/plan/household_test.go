package plan

import (
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The classification is all-or-nothing on purpose: "mostly routines" is
// the shape an escalation would take (ADR-W030 A8.1).
func TestClassifyIsAllOrNothing(t *testing.T) {
	step := func(k resource.Kind) PlanStep { return PlanStep{Kind: k, Name: "x"} }
	for name, tc := range map[string]struct {
		steps []PlanStep
		want  string
	}{
		"no steps":         {nil, ClassEmpty},
		"one routine":      {[]PlanStep{step(resource.KindAutomation)}, ClassHouseholdRoutine},
		"several routines": {[]PlanStep{step(resource.KindAutomation), step(resource.KindAutomation)}, ClassHouseholdRoutine},
		"a container":      {[]PlanStep{step(resource.KindContainer)}, ClassInfrastructure},
		"identity":         {[]PlanStep{step(resource.KindLdapGroup)}, ClassInfrastructure},
		"a routine and a route": {
			[]PlanStep{step(resource.KindAutomation), step(resource.KindRoute)}, ClassMixed},
		"a route and a routine": {
			[]PlanStep{step(resource.KindRoute), step(resource.KindAutomation)}, ClassMixed},
	} {
		t.Run(name, func(t *testing.T) {
			p := &Plan{Steps: tc.steps}
			if got := Classify(p); got != tc.want {
				t.Fatalf("Classify = %q, want %q", got, tc.want)
			}
			if got := ReviewableByHousehold(p); got != (tc.want == ClassHouseholdRoutine) {
				t.Fatalf("ReviewableByHousehold = %t for a %s plan", got, tc.want)
			}
		})
	}
	// A nil plan is not reviewable by anyone.
	if ReviewableByHousehold(nil) {
		t.Fatal("a nil plan was reviewable by the household")
	}
}
