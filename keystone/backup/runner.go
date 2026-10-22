package backup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/reconcile"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// ExitError is a run whose container exited with a non-zero code. The
// engine reads Code with errors.As to tell restic's verdicts apart (see
// resticExit): "repository does not exist" is a reason to init, "wrong
// password" is a reason to stop.
type ExitError struct {
	Name string
	Code int32
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s: exited %d", e.Name, e.Code)
}

// exitCode returns the container exit code err carries, and whether it
// carries one at all.
func exitCode(err error) (int32, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

// ApplierRunner is the production Runner: EnsureContainer through the
// same closed op surface every other container takes (the agent's
// ownership labels and guards apply unchanged), poll InspectContainer
// until the one-shot exits, then remove it. Success is the container's
// own exit code 0 (InspectContainerResult.exit_code, protocol minor 11);
// a non-zero code is an *ExitError. A container still running at Timeout
// is treated as failed and removed.
type ApplierRunner struct {
	Applier reconcile.Applier
	// Poll is the completion-poll interval. <= 0 uses 2s.
	Poll time.Duration
	// Timeout bounds one run. <= 0 uses 30 minutes. A deadline on the
	// run's context replaces it.
	Timeout time.Duration
	// GuardEvery is how often a run's guard (see withGuard) is checked
	// while the container runs. <= 0 uses 30s.
	GuardEvery time.Duration
}

func (a *ApplierRunner) RunContainer(ctx context.Context, node, name string, spec resource.ContainerSpec) error {
	// Pull before ensure: the agent's ensureContainer never pulls, and a
	// digest-pinned restic image is never resolved implicitly, so a
	// fresh node's first backup run would fail at create otherwise
	// (found by the first real VM install; the exit drill previously
	// masked this with a manual pre-pull).
	pres, err := a.Applier.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_PullImage{
		PullImage: &wsv1.PullImage{Image: spec.Image},
	}})
	if err != nil {
		return fmt.Errorf("pull %s: %w", spec.Image, err)
	}
	if oe := pres.GetError(); oe != nil {
		return fmt.Errorf("pull %s: %s: %s", spec.Image, oe.GetCode(), oe.GetMessage())
	}

	// Clear any leftover of the same name first (a Keystone that died
	// mid-run never reached its deferred remove). EnsureContainer keeps a
	// same-image container rather than recreating it, so a leftover would
	// hand back the PREVIOUS run's exit code as this run's verdict — or,
	// still running, have this run attach to its stale command, mounts
	// and credentials. So the clear-out must succeed; "nothing there" is
	// success.
	if err := a.clearLeftover(ctx, node, name); err != nil {
		return err
	}

	res, err := a.Applier.Apply(ctx, node, ensureOp(name, spec))
	if err != nil {
		return fmt.Errorf("ensure %s: %w", name, err)
	}
	if oe := res.GetError(); oe != nil {
		return fmt.Errorf("ensure %s: %s: %s", name, oe.GetCode(), oe.GetMessage())
	}
	defer a.remove(ctx, node, name)

	poll := a.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	guard := guardFrom(ctx)
	guardEvery := a.GuardEvery
	if guardEvery <= 0 {
		guardEvery = guardEvery30
	}
	nextGuard := time.Now().Add(guardEvery)
	deadline := time.Now().Add(timeout)
	// A caller that set its own deadline (an off-site run, bounded in
	// days rather than minutes) is the authority on how long this run
	// may take.
	if d, ok := ctx.Deadline(); ok {
		deadline = d
		timeout = time.Until(d).Round(time.Second)
	}
	for {
		res, err := a.Applier.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
			InspectContainer: &wsv1.InspectContainer{Name: name},
		}})
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if oe := res.GetError(); oe != nil {
			if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
				// Nothing Wildstone creates here auto-removes, so a vanished
				// container took its exit code with it: the run's outcome is
				// unknown, and unknown is never success.
				return fmt.Errorf("run %s: container disappeared before its exit code was read", name)
			}
			return fmt.Errorf("inspect %s: %s: %s", name, oe.GetCode(), oe.GetMessage())
		}
		if done, err := finished(name, res.GetInspectContainer()); done {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %s: still running after %s", name, timeout)
		}
		// The disk-space guard: stop restic before the disk fills, not
		// after. The deferred remove stops the container; restic's
		// partial pack files are cleaned by the next prune.
		if guard != nil && !time.Now().Before(nextGuard) {
			nextGuard = time.Now().Add(guardEvery)
			if err := guard(ctx); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// finished reports whether the one-shot has reached a terminal state and,
// if so, its outcome. It mirrors reconcile's classifyJob: "created",
// "paused" and "restarting" are not finished; "exited" and "dead" are,
// with the exit code as the verdict. An agent older than protocol minor
// 11 reports no status, so a stopped container's result cannot be known
// — that fails the run rather than guessing either way.
func finished(name string, inspect *wsv1.InspectContainerResult) (bool, error) {
	if inspect.GetRunning() {
		return false, nil
	}
	switch inspect.GetStatus() {
	case "":
		return true, fmt.Errorf("run %s: the node agent reports no exit status (needs protocol minor 11 or later)", name)
	case "exited", "dead":
		if code := inspect.GetExitCode(); code != 0 {
			return true, &ExitError{Name: name, Code: code}
		}
		return true, nil
	default:
		return false, nil
	}
}

// clearLeftover force-removes any container called name — running or
// not — and fails if it could not. A missing container is not an error
// (RemoveContainer answers existed=false).
func (a *ApplierRunner) clearLeftover(ctx context.Context, node, name string) error {
	res, err := a.Applier.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
		RemoveContainer: &wsv1.RemoveContainer{Name: name, Force: true},
	}})
	if err != nil {
		return fmt.Errorf("clearing a leftover %s: %w", name, err)
	}
	if oe := res.GetError(); oe != nil {
		return fmt.Errorf("clearing a leftover %s: %s: %s", name, oe.GetCode(), oe.GetMessage())
	}
	return nil
}

func (a *ApplierRunner) remove(ctx context.Context, node, name string) {
	a.Applier.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
		StopContainer: &wsv1.StopContainer{Name: name},
	}})
	a.Applier.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{ //nolint:errcheck
		RemoveContainer: &wsv1.RemoveContainer{Name: name, Force: true},
	}})
}

// ensureOp mirrors the reconcile engine's op builder for the fields a
// one-shot run uses. No spec-hash label is stamped: run containers are
// removed after each run, never reconciled against a desired spec.
func ensureOp(name string, spec resource.ContainerSpec) *wsv1.Operation {
	var mounts []*wsv1.Mount
	for _, m := range spec.Mounts {
		mounts = append(mounts, &wsv1.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return &wsv1.Operation{Op: &wsv1.Operation_EnsureContainer{EnsureContainer: &wsv1.EnsureContainer{
		Name: name, Image: spec.Image, Env: spec.Env, Mounts: mounts,
		RestartPolicy: spec.Restart, Command: spec.Command, Network: spec.RunNetwork,
	}}}
}
