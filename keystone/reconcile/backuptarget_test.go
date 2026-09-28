package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func backupTargetResource(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindBackupTarget,
		Metadata:   resource.Metadata{Name: name, Owner: "owner", Node: "node-a"},
		BackupTarget: &resource.BackupTargetSpec{
			Type: "s3", Endpoint: "https://s3.example.com", Bucket: "wildstone-backups",
			AccessKeySecret: "secret/offsite-access", SecretKeySecret: "secret/offsite-secret",
		},
	}
}

// TestReconcileBackupTargetNeedsApprovalThenIsDeclaredOnly: a BackupTarget
// is High, so unattended reconcile never applies it; once approved it is
// recorded with no op sent to the node, and re-reconciling is a no-op.
func TestReconcileBackupTargetNeedsApprovalThenIsDeclaredOnly(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	r := newReconciler(&GatewayApplier{Gateway: h.gw})
	desired := []resource.Resource{backupTargetResource("offsite")}

	held, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("unattended reconcile: %v", err)
	}
	if !held.PendingApproval || held.Plan.OverallRisk != plan.RiskHigh {
		t.Fatalf("unattended reconcile of a BackupTarget = %+v, want held for approval at High", held)
	}

	r.Approve = func(*plan.Plan) bool { return true }
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("approved reconcile: %v", err)
	}
	if !outcome.Applied || len(outcome.Steps) != 1 || !outcome.Steps[0].Applied || !outcome.Steps[0].HealthOK ||
		outcome.Steps[0].Step.Kind != resource.KindBackupTarget || outcome.Steps[0].Step.Action != plan.ActionCreate {
		t.Fatalf("approved reconcile = %+v, want one applied BackupTarget create", outcome)
	}

	again, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("re-reconcile: %v", err)
	}
	if !again.Applied || len(again.Plan.Steps) != 0 {
		t.Fatalf("re-reconcile produced steps: %+v", again.Plan.Steps)
	}
}
