package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func jobResource(name, image string, env map[string]string, mounts []resource.Mount) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindJob,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/identity", Node: "node-a"},
		Job: &resource.JobSpec{Run: resource.ContainerSpec{
			Image: image, Env: env, Mounts: mounts,
			Command: []string{"/app/bootstrap.sh"},
		}},
	}
}

func newJobReconciler(a Applier) *Reconciler {
	r := newReconciler(a)
	// The fake exits a job the instant it starts, so one poll is enough
	// and a slow gate would only make the suite slow.
	r.JobGateAttempts = 3
	r.JobGateBackoff = 5 * time.Millisecond
	// A Job is Medium risk, so a nil Approve is default-deny and nothing
	// runs (ADR-W004 A3). That is the correct production posture and is
	// asserted on its own below; these tests are about what happens once
	// a run IS approved.
	r.Approve = func(*plan.Plan) bool { return true }
	return r
}

// TestJobNeedsExplicitApproval states the posture the other tests opt
// out of. Every other additive create in this system is Low risk and
// applies unattended; a Job is not, because applying one RUNS a program
// that changes an external system and nothing reverses it. A homelab
// that seeds its directory without anyone saying yes is the wrong
// default.
func TestJobNeedsExplicitApproval(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setJobExit("bootstrap", 0)

	r := newReconciler(&GatewayApplier{Gateway: h.gw}) // no Approve: default-deny
	outcome, err := r.Reconcile(context.Background(), "node-a",
		[]resource.Resource{jobResource("bootstrap", testDigestImage, nil, nil)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.PendingApproval {
		t.Errorf("a Job plan applied without approval: %+v", outcome)
	}
	if got := agent.jobRunCount("bootstrap"); got != 0 {
		t.Errorf("the job ran %d times without approval, want 0", got)
	}
}

// TestJobRunsOnceAndThenStopsRunning is the property the whole kind
// exists for. A provisioning run must happen, and must NOT happen again
// on every tick after it succeeded — the second is what makes a job
// unusable in a level-triggered loop, because "seed the directory"
// running forever is not idempotent in any way you can rely on.
func TestJobRunsOnceAndThenStopsRunning(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setJobExit("lldap-bootstrap", 0)

	desired := []resource.Resource{jobResource("lldap-bootstrap", testDigestImage, nil, nil)}
	r := newJobReconciler(&GatewayApplier{Gateway: h.gw})

	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.Incident != "" {
		t.Fatalf("first tick did not apply: %+v", outcome)
	}
	if got := agent.jobRunCount("lldap-bootstrap"); got != 1 {
		t.Fatalf("job ran %d times on the first tick, want 1", got)
	}

	// Three more ticks with the same desired state. A completed job is
	// observed as present, so no step is planned and nothing re-runs.
	for i := 0; i < 3; i++ {
		outcome, err = r.Reconcile(context.Background(), "node-a", desired)
		if err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
		if len(outcome.Plan.Steps) != 0 {
			t.Errorf("tick %d planned %d steps for a completed job, want 0: %+v",
				i+2, len(outcome.Plan.Steps), outcome.Plan.Steps)
		}
	}
	if got := agent.jobRunCount("lldap-bootstrap"); got != 1 {
		t.Errorf("job ran %d times across four ticks, want 1 — a completed run is being repeated", got)
	}
}

// TestFailedJobRetriesUntilItSucceeds is the other half, and the reason
// observe reports a failed job as drifted rather than done. A job whose
// service was not ready yet must run again; this is the ONLY retry
// mechanism, deliberately, so that ordering never has to be modelled as
// a dependency edge.
func TestFailedJobRetriesUntilItSucceeds(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setJobExit("bootstrap", 1) // the directory is not up yet

	desired := []resource.Resource{jobResource("bootstrap", testDigestImage, nil, nil)}
	r := newJobReconciler(&GatewayApplier{Gateway: h.gw})

	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome.Applied {
		t.Error("a job that exited non-zero was reported as applied: a failed provisioning run must not " +
			"look like a successful one")
	}
	var sawExit bool
	for _, sr := range outcome.Steps {
		if strings.Contains(sr.Error, "exited 1") {
			sawExit = true
		}
	}
	if !sawExit {
		t.Errorf("no step error named the exit code; steps: %+v", outcome.Steps)
	}

	// Second tick, still failing: it must try again rather than give up.
	if _, err := r.Reconcile(context.Background(), "node-a", desired); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := agent.jobRunCount("bootstrap"); got != 2 {
		t.Fatalf("job ran %d times over two failing ticks, want 2 — a failed job that stops retrying "+
			"leaves a half-provisioned system that fixing the cause will never converge", got)
	}

	// Whatever it was waiting for arrives.
	agent.setJobExit("bootstrap", 0)
	outcome, err = r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("the run that succeeded was not applied: %+v", outcome)
	}
	if got := agent.jobRunCount("bootstrap"); got != 3 {
		t.Fatalf("job ran %d times, want 3", got)
	}

	// And now it settles.
	if _, err := r.Reconcile(context.Background(), "node-a", desired); err != nil {
		t.Fatalf("fourth tick: %v", err)
	}
	if got := agent.jobRunCount("bootstrap"); got != 3 {
		t.Errorf("job ran %d times after succeeding, want 3 — it did not settle", got)
	}
}

// TestJobRerunsWhenItsConfigChanges is what makes a Job re-provision
// without anything scheduling it. The job's spec hash covers the managed
// files it mounts, so editing the group list a bootstrap reads is a spec
// change, and a spec change re-runs the job. Nothing anywhere decides
// "this needs re-running".
func TestJobRerunsWhenItsConfigChanges(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setJobExit("bootstrap", 0)

	mounts := []resource.Mount{{
		Source: testManagedPrefix + "/identity", Target: "/config", ReadOnly: true,
	}}
	groups := func(content string) resource.Resource {
		return resource.Resource{
			ApiVersion: resource.ApiVersion,
			Kind:       resource.KindConfigFile,
			Metadata:   resource.Metadata{Name: "groups", Owner: "capability/identity", Node: "node-a"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "groups.json", Content: content},
		}
	}
	theJob := jobResource("bootstrap", testDigestImage, nil, mounts)
	r := newJobReconciler(&GatewayApplier{Gateway: h.gw})

	if _, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{groups(`["family"]`), theJob}); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if got := agent.jobRunCount("bootstrap"); got != 1 {
		t.Fatalf("first tick ran the job %d times, want 1", got)
	}

	// Same inputs: settled.
	if _, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{groups(`["family"]`), theJob}); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := agent.jobRunCount("bootstrap"); got != 1 {
		t.Fatalf("the job re-ran on unchanged input (%d runs)", got)
	}

	// A new group is added to the file the job reads.
	if _, err := r.Reconcile(context.Background(), "node-a", []resource.Resource{groups(`["family","photos"]`), theJob}); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if got := agent.jobRunCount("bootstrap"); got != 2 {
		t.Errorf("the job ran %d times after its input file changed, want 2 — a bootstrap that does not "+
			"re-read a changed group list leaves the new group unprovisioned forever", got)
	}
}

// TestJobOrderedAfterTheContainerItProvisions: the first tick brings up
// the service and runs the job against it, in that order. Not a
// dependency edge — just the position that saves the first tick from a
// guaranteed failure.
func TestJobOrderedAfterTheContainerItProvisions(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	agent.setJobExit("bootstrap", 0)

	desired := []resource.Resource{
		jobResource("bootstrap", testDigestImage, nil, nil),
		containerResource("lldap", testDigestImage),
	}
	r := newJobReconciler(&GatewayApplier{Gateway: h.gw})
	outcome, err := r.Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var containerAt, jobAt = -1, -1
	for i, s := range outcome.Plan.Steps {
		switch s.Kind {
		case resource.KindContainer:
			containerAt = i
		case resource.KindJob:
			jobAt = i
		}
	}
	if containerAt < 0 || jobAt < 0 {
		t.Fatalf("expected both a Container and a Job step, got %+v", outcome.Plan.Steps)
	}
	if jobAt < containerAt {
		t.Errorf("the Job (step %d) is planned before the Container it provisions (step %d)", jobAt, containerAt)
	}
}

// TestJobWithNoExitStatusIsNeitherDoneNorFailed pins the pre-minor-11
// agent case. Guessing "succeeded" silently accepts a failed run;
// guessing "failed" re-runs a completed one forever. Neither is
// acceptable, so the step says so instead.
func TestJobWithNoExitStatusIsNeitherDoneNorFailed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inspect func() (running bool, status string, code int32)
		want    jobState
	}{
		{"pre-minor-11 agent, stopped", func() (bool, string, int32) { return false, "", 0 }, jobIndetermin},
		{"exited cleanly", func() (bool, string, int32) { return false, "exited", 0 }, jobSucceeded},
		{"exited with an error", func() (bool, string, int32) { return false, "exited", 3 }, jobFailed},
		{"still running", func() (bool, string, int32) { return true, "running", 0 }, jobRunning},
		{"created but not started", func() (bool, string, int32) { return false, "created", 0 }, jobRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, status, code := tc.inspect()
			got := classifyJob(inspectResultForTest("h", running, status, code), "h")
			if got != tc.want {
				t.Errorf("classifyJob = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJobStaleContainerIsReplacedNotResumed: a container left over from
// a DIFFERENT spec must never be mistaken for this job's completed run,
// whatever exit code it happens to carry.
func TestJobStaleContainerIsReplacedNotResumed(t *testing.T) {
	if got := classifyJob(inspectResultForTest("old-hash", false, "exited", 0), "new-hash"); got != jobStale {
		t.Errorf("a container from a previous spec classified as %v, want jobStale — its exit 0 says "+
			"nothing about the run this spec is asking for", got)
	}
}

// inspectResultForTest builds the minimal InspectContainerResult
// classifyJob reads.
func inspectResultForTest(hash string, running bool, status string, code int32) *wsv1.InspectContainerResult {
	return &wsv1.InspectContainerResult{
		Running: running, Status: status, ExitCode: code,
		Labels: map[string]string{specHashLabel: hash},
	}
}

// TestJobIsCreatedWithRestartNo pins the exact string the agent sees.
//
// This must assert "no" and not merely "not unless-stopped", because
// leaving it EMPTY is what shipped: validateJob refuses a restart policy
// on the spec, so an empty value looked correct from Keystone's side —
// and the agent maps empty to "unless-stopped", which is right for a
// service and precisely wrong for a run. A completed job restarted
// forever:
//
//	lldap-provision-groups  RestartPolicy=unless-stopped  ExitCode=0  RestartCount=11
func TestJobIsCreatedWithRestartNo(t *testing.T) {
	spec := &resource.JobSpec{Run: resource.ContainerSpec{Image: testDigestImage}}
	op := containerEnsureOp("bootstrap", jobRunSpec(spec), ContainerNetwork, "").GetEnsureContainer()
	if got := op.GetRestartPolicy(); got != "no" {
		t.Errorf("a Job is created with restart policy %q, want \"no\" — empty is not good enough, "+
			"the agent turns empty into \"unless-stopped\" and a job that exited 0 is restarted forever", got)
	}
}

// TestJobRestartIsNotInheritedFromTheSpec: even if a restart policy
// reached the reconciler (a resource built in code and never validated,
// say), the run must not carry it.
func TestJobRestartIsNotInheritedFromTheSpec(t *testing.T) {
	spec := &resource.JobSpec{Run: resource.ContainerSpec{Image: testDigestImage, Restart: "always"}}
	if got := jobRunSpec(spec).Restart; got != "no" {
		t.Errorf("jobRunSpec kept restart=%q from the spec", got)
	}
}
