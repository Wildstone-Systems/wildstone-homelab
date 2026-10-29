package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

type clusterMemory struct {
	present        bool
	matches        bool
	failApply      bool
	partialService bool
	serviceOnly    bool
	lastApply      ClusterRequest
	lastDelete     ClusterRequest
	revision       string
	rv             string
}

func (m *clusterMemory) Apply(_ context.Context, req ClusterRequest) (ClusterResult, error) {
	switch req.Op {
	case OpWorkloadStatus:
		if !m.present {
			return ClusterResult{}, nil
		}
		serviceUID := "svc-1"
		if m.partialService {
			serviceUID = ""
		}
		uid, deploymentRV := "uid-1", m.rv
		if m.serviceOnly {
			uid, deploymentRV = "", ""
		}
		return ClusterResult{Found: true, UID: uid, ResourceVersion: deploymentRV, Revision: m.revision,
			ServiceUID: serviceUID, ServiceResourceVersion: m.rv, MatchesDesired: m.matches,
			Generation: 1, ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1}, nil
	case OpApplyWorkload:
		m.lastApply = req
		if m.failApply {
			m.failApply = false
			return ClusterResult{}, errors.New("leader term ended")
		}
		m.present, m.matches, m.partialService, m.serviceOnly, m.revision, m.rv = true, true, false, false, req.Authority.AcceptedRevision, "rv-1"
		return ClusterResult{Found: true, UID: "uid-1", ResourceVersion: m.rv}, nil
	case OpDeleteWorkload:
		m.lastDelete = req
		if m.serviceOnly {
			if req.UID != "" || req.ResourceVersion != "" || req.ServiceUID != "svc-1" || req.ServiceResourceVersion != m.rv {
				return ClusterResult{}, errors.New("stale service-only delete")
			}
		} else if req.UID != "uid-1" || req.ResourceVersion != m.rv {
			return ClusterResult{}, errors.New("stale delete")
		}
		m.present = false
		return ClusterResult{}, nil
	}
	return ClusterResult{}, errors.New("unknown op")
}

func clusterDesired() []resource.Resource {
	c := containerResource("it-tools", testDigestImage)
	c.Metadata.Owner, c.Metadata.Package = "capability/utilities", "tools-it-tools"
	c.Container.Backend = "k3s"
	r := routeResource("tools", "tools.example.test", "container/it-tools:80")
	r.Metadata.Owner, r.Metadata.Package = c.Metadata.Owner, c.Metadata.Package
	return []resource.Resource{c, r}
}

func TestClusterPlanApplyDriftDeleteAndLeaderRetry(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("node did not connect")
	}
	m := &clusterMemory{}
	newRec := func(epoch uint64, approve bool) *Reconciler {
		r := newReconciler(&GatewayApplier{Gateway: h.gw})
		r.Cluster = m
		r.ClusterRevision = "accepted-rev"
		r.Authorize = func(st plan.PlanStep) bool { return approve }
		r.ClusterAuthority = func(st plan.PlanStep) (Authority, error) {
			return Authority{AcceptedRevision: "accepted-rev", PlanID: "plan-211", StepDigest: "sha256:reviewed", LeaderEpoch: epoch}, nil
		}
		r.ClusterDelete = func(kind resource.Kind, name string) bool {
			return kind == resource.KindContainer && name == "it-tools"
		}
		return r
	}
	desired := clusterDesired()
	r := newRec(4, false)
	first, err := r.Reconcile(t.Context(), "node-a", desired)
	if err != nil {
		t.Fatal(err)
	}
	if !first.PendingApproval || m.present {
		t.Fatalf("unapproved cluster create applied: %+v", first)
	}
	r = newRec(4, true)
	m.failApply = true
	interrupted, err := r.Reconcile(t.Context(), "node-a", desired)
	if err != nil {
		t.Fatal(err)
	}
	if !interrupted.Retryable || m.present {
		t.Fatalf("leader interruption not retryable: %+v", interrupted)
	}
	r = newRec(5, true) // Keystone and the leader both restarted.
	created, err := r.Reconcile(t.Context(), "node-a", desired)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Applied || !m.present || m.lastApply.Authority.LeaderEpoch != 5 || m.lastApply.Authority.StepDigest != "sha256:reviewed" {
		t.Fatalf("approved cluster create = %+v, request %+v", created, m.lastApply)
	}
	// The Deployment landed with this accepted revision, but Service
	// creation was interrupted. The same approved Create must finish it.
	m.matches, m.partialService = false, true
	r = newRec(6, false)
	r.Authorize = func(st plan.PlanStep) bool { return st.Action == plan.ActionCreate }
	r.ClusterRoutes = desired[1:]
	m.revision = "another-revision"
	unrelated, err := r.Reconcile(t.Context(), "@cluster", desired[:1])
	if err != nil || !unrelated.PendingApproval || !m.partialService {
		t.Fatalf("unrelated partial workload used Create approval: outcome=%+v err=%v", unrelated, err)
	}
	m.revision = "accepted-rev"
	resumed, err := r.Reconcile(t.Context(), "@cluster", desired[:1])
	if err != nil || !resumed.Applied || m.partialService || m.lastApply.ExpectedRevision != "accepted-rev" {
		t.Fatalf("partial Create did not resume: outcome=%+v request=%+v err=%v", resumed, m.lastApply, err)
	}
	m.matches = false // An owned Deployment was hand-edited.
	r.Authorize = func(plan.PlanStep) bool { return false }
	drift, err := r.Reconcile(t.Context(), "node-a", desired)
	if err != nil {
		t.Fatal(err)
	}
	if !drift.PendingApproval || len(drift.Unauthorized) != 1 || drift.Unauthorized[0].Action != plan.ActionUpdate || m.matches {
		t.Fatalf("owned drift was silently repaired: %+v", drift)
	}
	r = newRec(6, true)
	r.SeedManaged("node-a", []ManagedRef{{Kind: resource.KindContainer, Name: "it-tools"}})
	deleted, err := r.Reconcile(t.Context(), "node-a", desired[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Applied || m.present || m.lastDelete.UID != "uid-1" || m.lastDelete.ResourceVersion != "rv-1" || m.lastDelete.Authority.LeaderEpoch != 6 {
		t.Fatalf("approved cluster delete = %+v, request %+v", deleted, m.lastDelete)
	}

	// If the Deployment disappeared independently, the surviving owned
	// Service still carries a complete preconditioned delete identity.
	m.present, m.serviceOnly, m.revision, m.rv = true, true, "accepted-rev", "rv-2"
	r = newRec(7, true)
	r.SeedManaged("node-a", []ManagedRef{{Kind: resource.KindContainer, Name: "it-tools"}})
	serviceOnlyDeleted, err := r.Reconcile(t.Context(), "node-a", desired[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !serviceOnlyDeleted.Applied || m.present || m.lastDelete.UID != "" || m.lastDelete.ResourceVersion != "" || m.lastDelete.ServiceUID != "svc-1" || m.lastDelete.ServiceResourceVersion != "rv-2" {
		t.Fatalf("approved Service-only delete = %+v, request %+v", serviceOnlyDeleted, m.lastDelete)
	}
}
