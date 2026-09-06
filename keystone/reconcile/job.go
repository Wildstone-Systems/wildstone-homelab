package reconcile

// Job applier and observer: a container run ONCE to completion.
//
// Provisioning is the gap this closes. A directory's users and groups,
// an application's first admin account, a setup wizard that has to be
// answered before an API turns on — none of those are expressible as
// desired state, because they are things a running system DID, not
// things it is. Every one is reached by running a short program against
// an already-running service.
//
// The reconciler's whole model is level-triggered: observe the world,
// diff against desired, apply. A run does not fit that shape on its own
// — "has this run happened" is not a property of the world you can read
// off a container. What makes it fit is the exit status: protocol minor
// 11 lets the agent say "exited 0" rather than only "not running", and
// THAT is a property of the world. A completed job is a container in a
// specific observable state, so a Job becomes an ordinary level-
// triggered resource after all.
//
// Re-running falls out of the spec hash rather than being scheduled.
// A Job's identity is its spec hash, which since the config-digest
// change covers the managed files it mounts. Change the group list a
// bootstrap job reads and the hash changes; the old completed container
// no longer matches desired; observe reports drift; the job runs again
// with the new input. Nothing anywhere decides "this needs re-running".
//
// It is deliberately NOT an Exec op. ADR-W003 makes a generic execution
// primitive a rejected change by policy, and this is not one: a Job
// names a digest-pinned image and runs under the same ownership, mount,
// network and validation rules as any other container. The privileged
// surface does not grow — a Job is applied with the EnsureContainer the
// agent already has.

import (
	"context"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// jobRunSpec is the ContainerSpec a Job is applied as.
//
// Restart is forced to "no" — explicitly, not left empty. Empty is what
// this used to do, on the reasoning that validateJob refuses a restart
// policy anyway, and it was wrong for a reason that is invisible from
// here: the AGENT maps an empty restart policy to "unless-stopped"
// (ops.go), which is the right default for a service and exactly wrong
// for a run. A job that exited 0 was restarted forever:
//
//	lldap-provision-groups  RestartPolicy=unless-stopped  ExitCode=0  RestartCount=11
//
// So the reconciler states it. Wildstone owns when a job runs again, and
// it decides that from the exit status, not from a policy on the node.
func jobRunSpec(spec *resource.JobSpec) *resource.ContainerSpec {
	run := spec.Run
	run.Restart = "no"
	return &run
}

// jobState is what observe needs to know about a job's container, read
// from one InspectContainer.
type jobState int

const (
	jobAbsent     jobState = iota // no container by that name
	jobStale                      // present, but for a different spec
	jobRunning                    // this spec's run is in flight
	jobSucceeded                  // this spec's run exited 0
	jobFailed                     // this spec's run exited non-zero
	jobIndetermin                 // present, this spec, but the agent cannot say (pre-minor-11)
)

// classifyJob maps an InspectContainerResult onto jobState for a desired
// spec hash.
//
// The pre-minor-11 case is why this is a function and not three ifs. An
// older agent leaves status empty, so "not running" is all it can say —
// and a job that has finished successfully looks exactly like one that
// crashed. Guessing either way is wrong in a way that matters: guess
// "succeeded" and a failed provisioning run is silently accepted; guess
// "failed" and a completed job re-runs on every tick forever. So the
// answer is neither, and the step reports why.
func classifyJob(inspect *wsv1.InspectContainerResult, wantHash string) jobState {
	if inspect.GetLabels()[specHashLabel] != wantHash {
		return jobStale
	}
	if inspect.GetRunning() {
		return jobRunning
	}
	switch inspect.GetStatus() {
	case "":
		return jobIndetermin
	case "exited", "dead":
		if inspect.GetExitCode() == 0 {
			return jobSucceeded
		}
		return jobFailed
	default:
		// "created", "paused", "restarting": not running, not finished.
		return jobRunning
	}
}

// observeJob reports a Job as present ONLY when this exact spec's run
// completed successfully. Every other state — absent, stale, in flight,
// failed — comes back as a deliberately-unequal placeholder, which
// plans an Update and re-runs it. That is the retry mechanism, and the
// only one: a job whose service is not up yet exits non-zero, and the
// next tick tries again.
//
// The cost of that bargain, stated plainly: a job that can never
// succeed is retried on every tick forever. The loop's own failure
// backoff bounds the noise, and the step error names the exit code each
// time. It is the right side of the trade because the alternative — a
// job that gives up — leaves a half-provisioned system that no amount
// of fixing the underlying problem will converge.
func (r *Reconciler) observeJob(ctx context.Context, nodeID string, k resKey, want resource.Resource, isDesired bool, cfgContents map[string]string) (resource.Resource, bool, error) {
	res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
		InspectContainer: &wsv1.InspectContainer{Name: k.Name},
	}})
	if err != nil {
		return resource.Resource{}, false, fmt.Errorf("observe job %s: %w", k.Name, err)
	}
	if oe := res.GetError(); oe != nil {
		switch oe.GetCode() {
		case wsv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			if !isDesired {
				return resource.Resource{}, false, nil // absent and unwanted: nothing to do
			}
			return placeholderResource(resource.KindJob, k.Name), true, nil
		case wsv1.ErrorCode_ERROR_CODE_FORBIDDEN:
			// A foreign container wearing this job's name. Never planned
			// against, exactly as observeContainer treats one.
			return resource.Resource{}, false, nil
		case wsv1.ErrorCode_ERROR_CODE_INVALID:
			// An agent that predates InspectContainer (protocol minor
			// 5). It cannot answer whether this job ran, but that is no
			// reason to abort observation of the WHOLE node: returning
			// an error here means one unobservable Job stops every
			// container, route and volume on that node from being
			// reconciled. observeContainer degrades in this situation
			// rather than failing, and so does this.
			//
			// Degrading means reporting drift, which plans an Update and
			// re-runs the job. The run itself works on such an agent —
			// only the confirmation is missing — and applyJobStep's gate
			// reports jobIndetermin with the actionable message. Loud
			// and converging beats quiet and stuck; the provisioning
			// scripts are idempotent, which is what makes it affordable.
			if !isDesired {
				return resource.Resource{}, false, nil
			}
			return placeholderResource(resource.KindJob, k.Name), true, nil
		default:
			return resource.Resource{}, false, fmt.Errorf("observe job %s: %s: %s", k.Name, oe.GetCode(), oe.GetMessage())
		}
	}
	if !isDesired {
		// The container exists but no Job wants it: report it present so
		// a Delete is planned and the finished container is cleaned up.
		return placeholderResource(resource.KindJob, k.Name), true, nil
	}

	wantHash := specHash(jobRunSpec(want.Job), ContainerNetwork, configDigest(r.ManagedPrefix, jobRunSpec(want.Job), cfgContents))
	if classifyJob(res.GetInspectContainer(), wantHash) == jobSucceeded {
		return want, true, nil
	}
	return placeholderResource(resource.KindJob, k.Name), true, nil
}

// applyJobStep runs the job and waits for it to finish.
//
// EnsureContainer does the right thing in all three shapes without any
// special casing here: absent -> create and start; present with a
// different spec hash -> replace; present with a matching hash but
// stopped -> start, which re-runs the same command. The last is what
// makes a failed job retry rather than needing a remove first.
func (r *Reconciler) applyJobStep(ctx context.Context, nodeID string, step plan.PlanStep, desiredByKey map[resKey]resource.Resource, cfgContents map[string]string) StepResult {
	sr := StepResult{Step: step}
	key := resKey{step.Kind, step.Name}

	switch step.Action {
	case plan.ActionCreate, plan.ActionUpdate:
		want, ok := desiredByKey[key]
		if !ok || want.Job == nil {
			sr.Error = "internal: no desired Job spec for " + step.Name
			return sr
		}
		run := jobRunSpec(want.Job)
		if ne := r.ensureNodePrereqs(ctx, nodeID); ne != "" {
			sr.Error = ne
			return sr
		}
		if pr := r.pullImage(ctx, nodeID, run.Image); pr != "" {
			sr.Error = pr
			return sr
		}
		digest := configDigest(r.ManagedPrefix, run, cfgContents)
		res, err := r.retryDispatch(ctx, nodeID, containerEnsureOp(step.Name, run, ContainerNetwork, digest))
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		if oe := res.GetError(); oe != nil {
			sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
			return sr
		}
		sr.Applied = true

		state, exit := r.waitJobFinished(ctx, nodeID, step.Name, specHash(run, ContainerNetwork, digest))
		switch state {
		case jobSucceeded:
			sr.HealthOK = true
			r.markManaged(nodeID, key, true)
		case jobFailed:
			sr.Error = fmt.Sprintf("job %s exited %d — the run did not provision anything, and the next "+
				"tick will try again", step.Name, exit)
		case jobIndetermin:
			sr.Error = fmt.Sprintf("job %s finished but the agent did not report an exit status "+
				"(protocol minor < 11): whether it provisioned anything is unknown, and guessing either "+
				"way is worse than saying so — upgrade the agent on this node", step.Name)
		default:
			sr.Error = fmt.Sprintf("job %s did not finish within the health gate; it may still be "+
				"running, and the next tick will observe it", step.Name)
		}
		// Deliberately NOT reversed on failure, unlike a Container. A
		// job's container is the only record that the run happened and
		// how it ended; removing it would destroy the exit status and
		// the logs at the exact moment somebody needs them, and there is
		// nothing to roll back to — whatever the run did to the external
		// system before failing is already done.
		return sr

	case plan.ActionDelete:
		r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
			StopContainer: &wsv1.StopContainer{Name: step.Name},
		}})
		res, err := r.retryDispatch(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
			RemoveContainer: &wsv1.RemoveContainer{Name: step.Name, Force: true},
		}})
		if err != nil {
			sr.Error = err.Error()
			return sr
		}
		if oe := res.GetError(); oe != nil && oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
			sr.Error = fmt.Sprintf("%s: %s", oe.GetCode(), oe.GetMessage())
			return sr
		}
		sr.Applied = true
		sr.HealthOK = true
		r.markManaged(nodeID, key, false)
		return sr

	default:
		sr.Error = "unexpected action " + step.Action.String()
		return sr
	}
}

// JobGateAttempts and JobGateBackoff bound the wait for a job to finish.
// They are separate from the container health gate because the jobs are
// a different shape of wait: a container is expected to be up in
// milliseconds, while a provisioning run legitimately takes seconds —
// lldap's bootstrap walks two directories of JSON over GraphQL.
//
// Timing out is not a failure. The container keeps running, and the next
// tick observes whatever it settled on; the gate only bounds how long
// one apply holds the loop.
const (
	defaultJobGateAttempts = 60
	defaultJobGateBackoff  = time.Second
)

func (r *Reconciler) jobGateParams() (int, time.Duration) {
	attempts, backoff := r.JobGateAttempts, r.JobGateBackoff
	if attempts <= 0 {
		attempts = defaultJobGateAttempts
	}
	if backoff <= 0 {
		backoff = defaultJobGateBackoff
	}
	return attempts, backoff
}

// waitJobFinished polls until the job's container reaches a terminal
// state, returning that state and its exit code.
func (r *Reconciler) waitJobFinished(ctx context.Context, nodeID, name, wantHash string) (jobState, int32) {
	attempts, backoff := r.jobGateParams()
	for i := 0; i < attempts; i++ {
		res, err := r.Applier.Apply(ctx, nodeID, &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
			InspectContainer: &wsv1.InspectContainer{Name: name},
		}})
		if err == nil && res.GetError() == nil {
			inspect := res.GetInspectContainer()
			switch st := classifyJob(inspect, wantHash); st {
			case jobSucceeded, jobFailed, jobIndetermin:
				return st, inspect.GetExitCode()
			}
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return jobRunning, 0
			case <-time.After(backoff):
			}
		}
	}
	return jobRunning, 0
}
