package reconcile

import (
	"context"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func automationResource(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindAutomation,
		Metadata:   resource.Metadata{Name: name, Owner: "household"},
		Automation: &resource.AutomationSpec{
			Description: "test rule",
			Trigger:     resource.AutomationTrigger{EventType: "person.left_home"},
			Action:      resource.AutomationAction{Tool: "notifications.notify", Tier: "domestic"},
		},
	}
}

// An Automation is Keystone-owned (ADR-W024 A1): it is applied nowhere,
// so a pass over one converges with no steps, needs no Applier and no
// Provider, and never asks for approval — the plan that put it in the
// Statefile was the whole act.
func TestReconcileAutomationIsAlreadyConverged(t *testing.T) {
	r := &Reconciler{} // no Applier, no Providers: nothing may be dispatched
	out, err := r.Reconcile(context.Background(), "", []resource.Resource{automationResource("breakfast-check")})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !out.Applied || out.PendingApproval || out.Incident != "" || len(out.Plan.Steps) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	// Removing it is likewise no step: nothing was ever managed for it.
	out, err = r.Reconcile(context.Background(), "", nil)
	if err != nil || !out.Applied || len(out.Plan.Steps) != 0 {
		t.Fatalf("after removal: %+v, %v", out, err)
	}
}
