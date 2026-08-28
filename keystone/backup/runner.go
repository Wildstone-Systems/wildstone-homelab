package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// ApplierRunner is the production Runner: EnsureContainer through the
// same closed op surface every other container takes (the agent's
// ownership labels and guards apply unchanged), poll InspectContainer
// until the one-shot exits, then remove it. The op surface has no exit-
// code arm yet, so "ran to completion" is the success signal (see the
// package doc); a container still running at Timeout is treated as failed
// and removed.
type ApplierRunner struct {
	Applier reconcile.Applier
	// Poll is the completion-poll interval. <= 0 uses 2s.
	Poll time.Duration
	// Timeout bounds one run. <= 0 uses 30 minutes.
	Timeout time.Duration
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
	deadline := time.Now().Add(timeout)
	for {
		res, err := a.Applier.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_InspectContainer{
			InspectContainer: &wsv1.InspectContainer{Name: name},
		}})
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if oe := res.GetError(); oe != nil {
			if oe.GetCode() == wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
				return nil // exited and auto-removed
			}
			return fmt.Errorf("inspect %s: %s: %s", name, oe.GetCode(), oe.GetMessage())
		}
		if !res.GetInspectContainer().GetRunning() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("run %s: still running after %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
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
		RestartPolicy: spec.Restart, Command: spec.Command,
	}}}
}
