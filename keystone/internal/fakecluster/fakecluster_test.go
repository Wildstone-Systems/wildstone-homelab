package fakecluster

import (
	"context"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
)

func TestRecordsRequestsInOrderAndReportsConverged(t *testing.T) {
	c := &Cluster{Node: "node-a"}
	apply := reconcile.ClusterRequest{Op: reconcile.OpApplyWorkload, Name: "app", Workload: &reconcile.WorkloadSpec{Name: "app", Replicas: 2}}
	res, err := c.Apply(context.Background(), apply)
	if err != nil || res.ReadyReplicas != 2 || res.Replicas != 2 || res.Node != "node-a" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if _, err := c.Apply(context.Background(), reconcile.ClusterRequest{Op: reconcile.OpWorkloadStatus, Name: "app"}); err != nil {
		t.Fatal(err)
	}
	got := c.Requests()
	if len(got) != 2 || got[0].Op != reconcile.OpApplyWorkload || got[1].Op != reconcile.OpWorkloadStatus {
		t.Fatalf("requests = %+v", got)
	}
}
