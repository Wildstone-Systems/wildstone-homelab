package loop

import (
	"context"
	"encoding/json"
	"testing"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// ensureRecorder is okApplier that remembers which containers were
// (re)created on which node, and can refuse some.
type ensureRecorder struct {
	okApplier
	ensured map[string][]string // node -> container names
	refuse  map[string]bool
}

func (a *ensureRecorder) Apply(ctx context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	if ec := op.GetEnsureContainer(); ec != nil {
		if a.refuse[ec.GetName()] {
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{
				Code: wsv1.ErrorCode_ERROR_CODE_INVALID, Message: "refused",
			}}}, nil
		}
		if a.ensured == nil {
			a.ensured = map[string][]string{}
		}
		a.ensured[node] = append(a.ensured[node], ec.GetName())
	}
	return a.okApplier.Apply(ctx, node, op)
}

// boundedHarness desires one container per name -> node. The fake agent
// reports every container present but out of date, so each is a Medium
// update that needs approval.
func boundedHarness(t *testing.T, placement map[string]string) (*Loop, *runtimedb.DB, *ensureRecorder, map[string]resource.Resource) {
	t.Helper()
	l, state, db, _ := harness(t)
	app := &ensureRecorder{refuse: map[string]bool{}}
	l.Rec.Applier = app
	l.Rec.MaxApplyRetries = 1
	want := map[string]resource.Resource{}
	for name, node := range placement {
		c := container(name, node, testImage)
		want[name] = c
		if err := state.Put(c); err != nil {
			t.Fatal(err)
		}
	}
	return l, db, app, want
}

func approve(t *testing.T, db *runtimedb.DB, planJSON string) string {
	t.Helper()
	id, err := db.InsertPlan("op", "", "reviewed", "medium", planJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionPlan(id, runtimedb.PlanApproved, "op"); err != nil {
		t.Fatal(err)
	}
	return id
}

func stateOf(t *testing.T, db *runtimedb.DB, id string) runtimedb.PlanState {
	t.Helper()
	rec, err := db.GetPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec.State
}

func driftSteps(t *testing.T, db *runtimedb.DB, id string) []string {
	t.Helper()
	rec, err := db.GetPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	var stored plan.StoredProposal
	if err := json.Unmarshal([]byte(rec.PlanJSON), &stored); err != nil || stored.Plan == nil {
		t.Fatalf("drift plan %s: %v %+v", id, err, stored)
	}
	var out []string
	for _, st := range stored.Plan.Steps {
		if st.Digest == "" && st.Action != plan.ActionDelete {
			t.Fatalf("drift step %s/%s carries no digest", st.Kind, st.Name)
		}
		out = append(out, string(st.Kind)+"/"+st.Name+"/"+st.Action.String())
	}
	return out
}

// An approved update on one node cannot authorize unrelated Medium/High
// drift on another (contract §7, bounded approval). The reviewed work
// applies; the rest is held and proposed as its own exact preview.
func TestApprovalDoesNotCarryUnrelatedDriftOnAnotherNode(t *testing.T) {
	l, db, app, want := boundedHarness(t, map[string]string{"web": "n1", "cache": "n2"})
	id := approve(t, db, reviewedUpdate(t, want["web"]))

	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := app.ensured["n1"]; len(got) != 1 || got[0] != "web" {
		t.Fatalf("n1 ensured %v; want the reviewed web update", got)
	}
	if got := app.ensured["n2"]; len(got) != 0 {
		t.Fatalf("n2 ensured %v; the unrelated cache drift rode the approval", got)
	}
	if st := stateOf(t, db, id); st != runtimedb.PlanApplied {
		t.Fatalf("plan = %s, want applied", st)
	}
	if out.DriftProposed == "" {
		t.Fatal("the held drift was not proposed")
	}
	if got := driftSteps(t, db, out.DriftProposed); len(got) != 1 || got[0] != "Container/cache/Update" {
		t.Fatalf("drift proposal = %v; want exactly the cache update", got)
	}
}

// On the same node, nothing applies until every Medium/High step there is
// approved: the plan never applies in part, and never applies with
// unreviewed company. Approving the exact drift preview releases both.
func TestUnrelatedDriftOnTheSameNodeHoldsItUntilApproved(t *testing.T) {
	l, db, app, want := boundedHarness(t, map[string]string{"web": "n1", "cache": "n1"})
	id := approve(t, db, reviewedUpdate(t, want["web"]))

	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(app.ensured["n1"]) != 0 {
		t.Fatalf("n1 ensured %v with an unapproved step on the node", app.ensured["n1"])
	}
	if st := stateOf(t, db, id); st != runtimedb.PlanApplying {
		t.Fatalf("plan = %s, want still applying", st)
	}
	if len(out.Waiting) != 1 || out.Waiting[0] != id {
		t.Fatalf("waiting = %v, want [%s]", out.Waiting, id)
	}
	if got := driftSteps(t, db, out.DriftProposed); len(got) != 1 || got[0] != "Container/cache/Update" {
		t.Fatalf("drift proposal = %v; want exactly the cache update", got)
	}

	// The next tick sees the same held drift and does not re-propose it.
	again, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again.DriftProposed != "" {
		t.Fatalf("an unchanged drift was re-proposed as %s", again.DriftProposed)
	}

	// A human approves the exact preview.
	if err := db.TransitionPlan(out.DriftProposed, runtimedb.PlanApproved, "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := app.ensured["n1"]; len(got) != 2 {
		t.Fatalf("n1 ensured %v after both were approved; want web and cache", got)
	}
	for _, p := range []string{id, out.DriftProposed} {
		if st := stateOf(t, db, p); st != runtimedb.PlanApplied {
			t.Fatalf("plan %s = %s, want applied", p, st)
		}
	}
}

// A reviewed create that recomputes as an update is a different effect
// and is not covered (the strict action rule), even at the same content.
func TestReviewedCreateDoesNotCoverAnUpdate(t *testing.T) {
	l, db, app, want := boundedHarness(t, map[string]string{"web": "n1"})
	id := approve(t, db, reviewedPlan(t, nil, []resource.Resource{want["web"]}))

	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(app.ensured["n1"]) != 0 {
		t.Fatalf("ensured %v under a create approval for what is an update", app.ensured["n1"])
	}
	if st := stateOf(t, db, id); st != runtimedb.PlanApplying {
		t.Fatalf("plan = %s, want applying", st)
	}
	if got := driftSteps(t, db, out.DriftProposed); len(got) != 1 || got[0] != "Container/web/Update" {
		t.Fatalf("drift proposal = %v; want the web update as a fresh preview", got)
	}
}

// Content that changed after review is not covered: the approval is
// bound to the digest the reviewer saw.
func TestReviewedDigestMustMatchTheAcceptedContent(t *testing.T) {
	l, db, app, want := boundedHarness(t, map[string]string{"web": "n1"})
	reviewed := want["web"]
	spec := *reviewed.Container
	spec.Env = map[string]string{"MODE": "reviewed"}
	reviewed.Container = &spec
	approve(t, db, reviewedUpdate(t, reviewed))

	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(app.ensured["n1"]) != 0 {
		t.Fatalf("ensured %v at content nobody reviewed", app.ensured["n1"])
	}
}

// Several Plans in one pass each get their own verdict: one failing
// target does not fail a Plan that never touched it.
func TestEachPlanGetsItsOwnVerdict(t *testing.T) {
	l, db, app, want := boundedHarness(t, map[string]string{"web": "n1", "cache": "n2"})
	app.refuse["cache"] = true
	good := approve(t, db, reviewedUpdate(t, want["web"]))
	bad := approve(t, db, reviewedUpdate(t, want["cache"]))

	out, err := l.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Failed == "" {
		t.Fatalf("tick = %+v; want the cache failure reported", out)
	}
	if st := stateOf(t, db, good); st != runtimedb.PlanApplied {
		t.Fatalf("web plan = %s, want applied", st)
	}
	if st := stateOf(t, db, bad); st != runtimedb.PlanFailed {
		t.Fatalf("cache plan = %s, want failed", st)
	}
}

// A step recorded before digests existed covers nothing: migration does
// not invent authority for work nobody can show was reviewed.
func TestStepWithoutDigestCoversNothing(t *testing.T) {
	l, db, app, want := boundedHarness(t, map[string]string{"web": "n1"})
	p := plan.Diff([]resource.Resource{want["web"]}, nil)
	for i := range p.Steps {
		p.Steps[i].Action = plan.ActionUpdate
		p.Steps[i].Digest = ""
	}
	raw, err := json.Marshal(plan.StoredProposal{BasedOnHead: "head", Summary: "legacy", Plan: p})
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, string(raw))
	if _, err := l.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(app.ensured["n1"]) != 0 {
		t.Fatalf("ensured %v under a digest-less legacy step", app.ensured["n1"])
	}
}

// A digest survives the Statefile: a resource written, committed and read
// back through LoadAt has the digest it was reviewed at.
func TestDigestSurvivesTheStatefile(t *testing.T) {
	_, state, _, _ := harness(t)
	c := container("web", "n1", testImage)
	c.Container.Env = map[string]string{"B": "2", "A": "1"}
	c.Container.Mounts = []resource.Mount{{Source: "data", Target: "/data"}}
	if err := state.Put(c); err != nil {
		t.Fatal(err)
	}
	if err := state.Commit("test", "", "add web"); err != nil {
		t.Fatal(err)
	}
	head, err := state.Head()
	if err != nil {
		t.Fatal(err)
	}
	back, err := state.LoadAt(head)
	if err != nil || len(back) != 1 {
		t.Fatalf("LoadAt: %v %+v", err, back)
	}
	if got, want := plan.Digest(back[0]), plan.Digest(c); got != want || got == "" {
		t.Fatalf("digest after the round trip = %q, reviewed %q", got, want)
	}
}
