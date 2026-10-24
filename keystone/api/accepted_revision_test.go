package api

import (
	"net/http"
	"os/exec"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// acceptedHarness wires the Statefile to the runtime DB's accepted
// revision, as cmd/keystone does, so proposals and approvals see only
// accepted desired state.
func acceptedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.s.State.Accepted = h.s.DB.StatefileHead
	return h
}

func (h *harness) propose(summary, doc string) runtimedb.PlanRecord {
	h.t.Helper()
	var rec runtimedb.PlanRecord
	if resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans",
		proposalRequest{Summary: summary, Put: []string{doc}}, &rec); resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("propose %q = %d", summary, resp.StatusCode)
	}
	return rec
}

// handCommit commits a resource the way a person with repo access would,
// without Keystone accepting it.
func (h *harness) handCommit(res resource.Resource) {
	h.t.Helper()
	if err := h.s.State.Put(res); err != nil {
		h.t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=mika", "-c", "user.email=mika@example.invalid", "commit", "-qm", "by hand"},
	} {
		cmd := exec.Command("git", append([]string{"-c", "gc.auto=0"}, args...)...)
		cmd.Dir = h.s.State.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			h.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func handVolume(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindVolume,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/test", Node: "n1"},
		Volume:     &resource.VolumeSpec{Name: name},
	}
}

func (h *harness) approve(planID string) int {
	h.t.Helper()
	return h.do(runtimedb.RoleAdmin, "POST", "/api/v1/plans/"+planID+"/approve", nil, nil).StatusCode
}

func planState(t *testing.T, h *harness, id string) runtimedb.PlanState {
	t.Helper()
	rec, err := h.s.DB.GetPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.State
}

// A proposal is computed from the accepted tree, and an edit nobody
// committed is not part of it.
func TestProposalIgnoresUncommittedEdits(t *testing.T) {
	h := acceptedHarness(t)
	if code := h.approve(h.propose("a", containerYAML(t, "app-a", "reg.example/a@"+testDigest, nil)).ID); code != http.StatusOK {
		t.Fatalf("approve a = %d", code)
	}
	// An uncommitted file in the working tree.
	if err := h.s.State.Put(handVolume("scratch")); err != nil {
		t.Fatal(err)
	}
	rec := h.propose("b", containerYAML(t, "app-b", "reg.example/b@"+testDigest, nil))
	var view planView
	h.do(runtimedb.RoleOperator, "GET", "/api/v1/plans/"+rec.ID, nil, &view)
	if view.Plan == nil || len(view.Plan.Steps) != 1 || view.Plan.Steps[0].Name != "app-b" {
		t.Fatalf("plan = %+v; want exactly the app-b create", view.Plan)
	}
}

// Approval writes with `git add -A`, so it must not run on top of a
// commit ingestion has not checked, or of edits nobody committed. Both
// are a wait, not a stale Plan: it stays proposed and can be approved
// once the Statefile settles.
func TestApprovalWaitsForUnacceptedOrUncommittedEdits(t *testing.T) {
	h := acceptedHarness(t)
	if code := h.approve(h.propose("a", containerYAML(t, "app-a", "reg.example/a@"+testDigest, nil)).ID); code != http.StatusOK {
		t.Fatalf("approve a = %d", code)
	}
	accepted, _ := h.s.DB.StatefileHead()

	// Uncommitted edits.
	rec := h.propose("b", containerYAML(t, "app-b", "reg.example/b@"+testDigest, nil))
	if err := h.s.State.Put(handVolume("scratch")); err != nil {
		t.Fatal(err)
	}
	if code := h.approve(rec.ID); code != http.StatusConflict {
		t.Fatalf("approve on a dirty tree = %d, want 409", code)
	}
	if st := planState(t, h, rec.ID); st != runtimedb.PlanProposed {
		t.Fatalf("plan after dirty refusal = %s, want still proposed", st)
	}

	// The same edit, committed by hand but not yet accepted.
	h.handCommit(handVolume("scratch"))
	if code := h.approve(rec.ID); code != http.StatusConflict {
		t.Fatalf("approve over an unaccepted commit = %d, want 409", code)
	}
	if st := planState(t, h, rec.ID); st != runtimedb.PlanProposed {
		t.Fatalf("plan after unaccepted-commit refusal = %s, want still proposed", st)
	}
	if marker, _ := h.s.DB.StatefileHead(); marker != accepted {
		t.Fatalf("accepted revision moved to %s without ingestion", marker)
	}
	// Nothing hand-made reached desired state.
	resources, err := h.s.State.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if r.Metadata.Name == "scratch" {
			t.Fatalf("an unaccepted resource is desired state: %+v", resources)
		}
	}
}

// A Statefile with history and nothing accepted cannot be proposed
// against: there is no base to diff from.
func TestProposalWaitsForAnAcceptedRevision(t *testing.T) {
	h := acceptedHarness(t)
	h.handCommit(handVolume("data"))
	var rec runtimedb.PlanRecord
	resp := h.do(runtimedb.RoleOperator, "POST", "/api/v1/plans",
		proposalRequest{Summary: "a", Put: []string{containerYAML(t, "app-a", "reg.example/a@"+testDigest, nil)}}, &rec)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("propose with nothing accepted = %d, want 503", resp.StatusCode)
	}
}
