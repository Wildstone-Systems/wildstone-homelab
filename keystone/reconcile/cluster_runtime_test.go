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
	present           bool
	matches           bool
	failApply         bool
	partialService    bool
	serviceOnly       bool
	lastApply         ClusterRequest
	lastDelete        ClusterRequest
	revision          string
	serviceRevision   string
	planID            string
	stepDigest        string
	rv                string
	rolloutPending    bool
	generationPending bool
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
		serviceRevision := m.serviceRevision
		if serviceUID != "" && serviceRevision == "" {
			serviceRevision = m.revision
		}
		readyReplicas := 1
		if m.rolloutPending {
			readyReplicas = 0
		}
		observedGeneration := int64(1)
		if m.generationPending {
			observedGeneration = 0
		}
		return ClusterResult{Found: true, UID: uid, ResourceVersion: deploymentRV, Revision: m.revision, PlanID: m.planID, StepDigest: m.stepDigest,
			ServiceUID: serviceUID, ServiceResourceVersion: m.rv, ServiceRevision: serviceRevision, MatchesDesired: m.matches,
			Generation: 1, ObservedGeneration: observedGeneration, Replicas: 1, ReadyReplicas: readyReplicas}, nil
	case OpApplyWorkload:
		m.lastApply = req
		if m.failApply {
			m.failApply = false
			return ClusterResult{}, errors.New("leader term ended")
		}
		m.present, m.matches, m.partialService, m.serviceOnly, m.revision, m.serviceRevision, m.planID, m.stepDigest, m.rv = true, true, false, false, req.Authority.AcceptedRevision, req.Authority.AcceptedRevision, req.Authority.PlanID, req.Authority.StepDigest, "rv-1"
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

func TestClusterApplyCarriesSeparateObjectRevisionWitnesses(t *testing.T) {
	desired := clusterDesired()[:1]
	desired[0].Container.Expose = []uint32{8080}
	want := desired[0]
	k := resKey{want.Kind, want.Metadata.Name}
	m := &clusterMemory{present: true, revision: "rev-b", serviceRevision: "rev-a", rv: "rv-1"}
	r := newReconciler(nil)
	r.Cluster = m
	r.ClusterAuthority = func(plan.PlanStep) (Authority, error) {
		return Authority{AcceptedRevision: "rev-b", PlanID: "plan-211", StepDigest: "sha256:reviewed", LeaderEpoch: 7}, nil
	}
	step := plan.PlanStep{Action: plan.ActionUpdate, Kind: resource.KindContainer, Name: want.Metadata.Name}
	result := r.applyClusterContainerStep(t.Context(), "@cluster", step, map[resKey]resource.Resource{k: want}, desired, map[resKey]ClusterResult{k: {Found: true, Revision: "rev-b", ServiceRevision: "rev-a"}})
	if !result.Applied || m.lastApply.ExpectedRevision != "rev-b" || m.lastApply.ExpectedServiceRevision != "rev-a" {
		t.Fatalf("separate revision witnesses were not preserved: result=%+v request=%+v", result, m.lastApply)
	}
}

func TestClusterPartialUpdateKeepsItsApprovedUpdateAction(t *testing.T) {
	desired := clusterDesired()[:1]
	desired[0].Container.Expose = []uint32{8080}
	m := &clusterMemory{present: true, partialService: true, revision: "accepted-rev", rv: "rv-1"}
	r := newReconciler(nil)
	r.Cluster = m
	r.Authorize = func(st plan.PlanStep) bool { return st.Action == plan.ActionUpdate }
	r.ClusterAuthority = func(st plan.PlanStep) (Authority, error) {
		if st.Action != plan.ActionUpdate {
			t.Fatalf("cluster authority requested for %s, want Update", st.Action)
		}
		return Authority{AcceptedRevision: "accepted-rev", PlanID: "plan-211", StepDigest: "sha256:reviewed", LeaderEpoch: 7}, nil
	}

	outcome, err := r.Reconcile(t.Context(), "@cluster", desired)
	if err != nil || !outcome.Applied || outcome.PendingApproval || m.partialService || m.lastApply.Op != OpApplyWorkload {
		t.Fatalf("partial approved Update did not resume: outcome=%+v request=%+v err=%v", outcome, m.lastApply, err)
	}
}

func TestClusterRolloutWaitKeepsItsApprovedCreateAction(t *testing.T) {
	for _, tc := range []struct {
		name              string
		rolloutPending    bool
		generationPending bool
	}{
		{name: "replicas are not Ready", rolloutPending: true},
		{name: "generation is not observed", generationPending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := clusterDesired()[:1]
			desired[0].Container.Expose = []uint32{8080}
			m := &clusterMemory{present: true, matches: true, revision: "accepted-rev", serviceRevision: "accepted-rev", planID: "plan-211", stepDigest: "sha256:reviewed", rv: "rv-1", rolloutPending: tc.rolloutPending, generationPending: tc.generationPending}
			r := newReconciler(nil)
			r.Cluster = m
			r.Authorize = func(st plan.PlanStep) bool { return st.Action == plan.ActionCreate }
			r.ClusterCreateRecovery = func(st plan.PlanStep, planID, stepDigest string) bool {
				return st.Action == plan.ActionCreate && planID == "plan-211" && stepDigest == "sha256:reviewed"
			}
			r.ClusterAuthority = func(st plan.PlanStep) (Authority, error) {
				if st.Action != plan.ActionCreate {
					t.Fatalf("cluster authority requested for %s, want Create", st.Action)
				}
				return Authority{AcceptedRevision: "accepted-rev", PlanID: "plan-211", StepDigest: "sha256:reviewed", LeaderEpoch: 7}, nil
			}

			waiting, err := r.Reconcile(t.Context(), "@cluster", desired)
			if err != nil || !waiting.Retryable || waiting.PendingApproval || len(waiting.Steps) != 1 || waiting.Steps[0].Step.Action != plan.ActionCreate {
				t.Fatalf("matching rollout lost its approved Create while waiting: outcome=%+v err=%v", waiting, err)
			}

			m.rolloutPending, m.generationPending = false, false
			ready, err := r.Reconcile(t.Context(), "@cluster", desired)
			if err != nil || !ready.Applied || ready.PendingApproval || len(ready.Plan.Steps) != 0 {
				t.Fatalf("ready matching workload did not converge: outcome=%+v err=%v", ready, err)
			}
		})
	}
}

func TestClusterRolloutWaitKeepsItsApprovedUpdateAction(t *testing.T) {
	desired := clusterDesired()[:1]
	desired[0].Container.Expose = []uint32{8080}
	m := &clusterMemory{present: true, matches: true, revision: "accepted-rev", serviceRevision: "accepted-rev", rv: "rv-1", rolloutPending: true}
	r := newReconciler(nil)
	r.Cluster = m
	r.Authorize = func(st plan.PlanStep) bool { return st.Action == plan.ActionUpdate }
	r.ClusterAuthority = func(st plan.PlanStep) (Authority, error) {
		if st.Action != plan.ActionUpdate {
			t.Fatalf("cluster authority requested for %s, want Update", st.Action)
		}
		return Authority{AcceptedRevision: "accepted-rev", PlanID: "plan-211", StepDigest: "sha256:reviewed", LeaderEpoch: 7}, nil
	}

	waiting, err := r.Reconcile(t.Context(), "@cluster", desired)
	if err != nil || !waiting.Retryable || waiting.PendingApproval || len(waiting.Steps) != 1 || waiting.Steps[0].Step.Action != plan.ActionUpdate {
		t.Fatalf("matching rollout lost its approved Update while waiting: outcome=%+v err=%v", waiting, err)
	}
}

func TestClusterPartialCreateSurvivesUnrelatedAcceptedRevision(t *testing.T) {
	for _, tc := range []struct {
		name           string
		partialService bool
		serviceOnly    bool
	}{
		{name: "missing Service", partialService: true},
		{name: "missing Deployment", serviceOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := clusterDesired()[:1]
			desired[0].Container.Expose = []uint32{8080}
			m := &clusterMemory{present: true, partialService: tc.partialService, serviceOnly: tc.serviceOnly, revision: "create-rev", serviceRevision: "create-rev", planID: "plan-211", stepDigest: "sha256:reviewed", rv: "rv-1"}
			r := newReconciler(nil)
			r.Cluster = m
			r.Authorize = func(st plan.PlanStep) bool { return st.Action == plan.ActionCreate }
			r.ClusterCreateRecovery = func(st plan.PlanStep, planID, stepDigest string) bool {
				return st.Action == plan.ActionCreate && planID == "plan-211" && stepDigest == "sha256:reviewed"
			}
			r.ClusterAuthority = func(st plan.PlanStep) (Authority, error) {
				if st.Action != plan.ActionCreate {
					t.Fatalf("cluster authority requested for %s, want Create", st.Action)
				}
				return Authority{AcceptedRevision: "unrelated-new-rev", PlanID: "plan-211", StepDigest: "sha256:reviewed", LeaderEpoch: 7}, nil
			}

			outcome, err := r.Reconcile(t.Context(), "@cluster", desired)
			if err != nil || !outcome.Applied || outcome.PendingApproval || m.partialService || m.serviceOnly || m.lastApply.ExpectedRevision != "create-rev" || m.lastApply.Authority.AcceptedRevision != "unrelated-new-rev" {
				t.Fatalf("partial Create did not survive an unrelated accepted revision: outcome=%+v request=%+v err=%v", outcome, m.lastApply, err)
			}
		})
	}
}

func TestClusterPartialCreateRequiresExactPlanStepWitness(t *testing.T) {
	for _, tc := range []struct{ name, planID, stepDigest string }{
		{name: "missing witness"},
		{name: "different Plan", planID: "plan-other", stepDigest: "sha256:reviewed"},
		{name: "different step", planID: "plan-211", stepDigest: "sha256:other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := clusterDesired()[:1]
			desired[0].Container.Expose = []uint32{8080}
			m := &clusterMemory{present: true, partialService: true, revision: "create-rev", planID: tc.planID, stepDigest: tc.stepDigest, rv: "rv-1"}
			r := newReconciler(nil)
			r.Cluster = m
			r.Authorize = func(st plan.PlanStep) bool { return st.Action == plan.ActionCreate }
			r.ClusterCreateRecovery = func(st plan.PlanStep, planID, stepDigest string) bool {
				return st.Action == plan.ActionCreate && planID == "plan-211" && stepDigest == "sha256:reviewed"
			}
			outcome, err := r.Reconcile(t.Context(), "@cluster", desired)
			if err != nil || !outcome.PendingApproval || !m.partialService || m.lastApply.Op != "" {
				t.Fatalf("unrelated workload witness resumed Create: outcome=%+v request=%+v err=%v", outcome, m.lastApply, err)
			}
		})
	}
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
		r.Authorize = func(st plan.PlanStep) bool { return approve }
		r.ClusterCreateRecovery = func(st plan.PlanStep, planID, stepDigest string) bool {
			return st.Action == plan.ActionCreate && planID == "plan-211" && stepDigest == "sha256:reviewed"
		}
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
	m.planID = "another-plan"
	unrelated, err := r.Reconcile(t.Context(), "@cluster", desired[:1])
	if err != nil || !unrelated.PendingApproval || !m.partialService {
		t.Fatalf("unrelated partial workload used Create approval: outcome=%+v err=%v", unrelated, err)
	}
	m.revision = "accepted-rev"
	m.planID = "plan-211"
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
