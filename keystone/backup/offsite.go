package backup

// Off-site runs (ADR-W045 A3): a BackupPlan with an offsite block also
// sends its data to a BackupTarget — an S3-compatible bucket.
//
//	offsite-only:       sources ──backup──────────────────────▶ S3
//	local-and-offsite:  sources ──backup──▶ local repo ──copy──▶ S3
//
// restic encrypts before anything leaves the node, under the off-site
// password, which validation keeps distinct from the local one. The S3
// credentials and both passwords reach the run container only as
// secret/<name> references that Wardstone materializes (ADR-W012).
//
// Every S3 command was checked against the pinned restic and a real S3
// server before it was written down here: a missing bucket or repository
// probes as exit 10 (and init creates the bucket), a wrong password as
// 12, a wrong key as 1. `restic copy` must lock the SOURCE repository, so
// the local repo is mounted read-write for it — read-only, restic retries
// the lock forever instead of failing.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// hasLocal reports whether a plan keeps a local repository: every plan
// except an offsite-only one.
func hasLocal(p *resource.BackupPlanSpec) bool {
	return p.Offsite == nil || p.Offsite.Route != resource.BackupRouteOffsiteOnly
}

func (e *Engine) offsiteTimeout() time.Duration {
	if e.OffsiteTimeout > 0 {
		return e.OffsiteTimeout
	}
	return 72 * time.Hour
}

// findTarget returns the BackupTarget a plan's off-site block names. The
// policy engine refuses a plan whose target is missing or on another
// node, so failing here means the Statefile was edited around policy.
func findTarget(desired []resource.Resource, plan resource.Resource) (*resource.BackupTargetSpec, error) {
	name := plan.BackupPlan.Offsite.Target
	for _, r := range desired {
		if r.Kind == resource.KindBackupTarget && r.Metadata.Name == name && r.BackupTarget != nil {
			if r.Metadata.Node != plan.Metadata.Node {
				return nil, fmt.Errorf("BackupTarget %q is on node %q, not %q", name, r.Metadata.Node, plan.Metadata.Node)
			}
			return r.BackupTarget, nil
		}
	}
	return nil, fmt.Errorf("no BackupTarget %q in the Statefile", name)
}

// offsiteRepository is restic's URL for a plan's off-site repository:
// one folder per plan under the target's bucket and prefix, so plans
// never share a repository (or a password).
func offsiteRepository(t *resource.BackupTargetSpec, plan string) string {
	parts := []string{strings.TrimSuffix(t.Endpoint, "/"), t.Bucket}
	if t.Prefix != "" {
		parts = append(parts, t.Prefix)
	}
	return "s3:" + strings.Join(append(parts, plan), "/")
}

// offsiteSpec composes one off-site restic pass. sources are mounted
// read-only under /backup/<vol>, as for a local backup. fromLocal adds
// the plan's local repository as restic's copy source, mounted
// read-write (see the file comment).
func (e *Engine) offsiteSpec(plan *resource.BackupPlanSpec, t *resource.BackupTargetSpec, planName string, command, sources []string, fromLocal bool) resource.ContainerSpec {
	env := map[string]string{
		"RESTIC_REPOSITORY":     offsiteRepository(t, planName),
		"RESTIC_PASSWORD":       plan.Offsite.PasswordSecret,
		"AWS_ACCESS_KEY_ID":     t.AccessKeySecret,
		"AWS_SECRET_ACCESS_KEY": t.SecretKeySecret,
	}
	if t.Region != "" {
		env["AWS_DEFAULT_REGION"] = t.Region
	}
	var mounts []resource.Mount
	if fromLocal {
		env["RESTIC_FROM_REPOSITORY"] = "/repo"
		env["RESTIC_FROM_PASSWORD"] = plan.PasswordSecret
		mounts = append(mounts, resource.Mount{Source: plan.Repository, Target: "/repo"})
	}
	for _, v := range sources {
		mounts = append(mounts, resource.Mount{Source: v, Target: "/backup/" + v, ReadOnly: true})
	}
	cmd := append([]string{}, command...)
	if t.PathStyle {
		cmd = append(cmd, "-o", "s3.bucket-lookup=path")
	}
	return resource.ContainerSpec{
		Image:   e.ResticImage,
		Env:     env,
		Mounts:  mounts,
		Command: cmd,
		Restart: "no",
	}
}

// runOffsite executes one off-site run with full provenance: make sure
// the off-site repository exists, send the data by the plan's route, and
// apply the off-site retention. Bounded by OffsiteTimeout rather than
// the runner's default, because a first upload of a large library can
// take days.
func (e *Engine) runOffsite(ctx context.Context, r resource.Resource, desired []resource.Resource) error {
	spec := r.BackupPlan
	name := r.Metadata.Name
	o := spec.Offsite
	return e.withProvenance(ctx, r, runtimedb.BackupRunOffsite,
		fmt.Sprintf("off-site run: %s (%s -> BackupTarget %s)", name, o.Route, o.Target),
		func(ctx context.Context) error {
			t, err := findTarget(desired, r)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, e.offsiteTimeout())
			defer cancel()
			node := r.Metadata.Node
			local := o.Route == resource.BackupRouteLocalAndOffsite

			probe := e.offsiteSpec(spec, t, name, []string{"cat", "config"}, nil, false)
			if err := e.Run.RunContainer(ctx, node, "offsite-"+name+"-probe", probe); err != nil {
				if code, ok := exitCode(err); !ok || code != resticExitNoRepo {
					return describe("off-site repository check", err)
				}
				// Matching chunker parameters is what keeps a copied
				// snapshot deduplicated off-site: without them every
				// copied file would be split differently and re-uploaded.
				initCmd := []string{"init"}
				if local {
					initCmd = append(initCmd, "--copy-chunker-params")
				}
				initSpec := e.offsiteSpec(spec, t, name, initCmd, nil, local)
				if err := e.Run.RunContainer(ctx, node, "offsite-"+name+"-init", initSpec); err != nil {
					return describe("off-site repository init", err)
				}
				e.log().Info("backup: initialized an off-site repository", "plan", name, "target", o.Target)
			}

			send, pass := e.offsiteSpec(spec, t, name, append([]string{"backup"}, sourceTargets(spec)...), spec.Volumes, false), "off-site backup pass"
			if local {
				send, pass = e.offsiteSpec(spec, t, name, []string{"copy"}, nil, true), "off-site copy pass"
			}
			if err := e.Run.RunContainer(ctx, node, "offsite-"+name, send); err != nil {
				return describe(pass, err)
			}

			forgetArgs := retentionArgs(o.Retention)
			if forgetArgs == nil {
				return nil
			}
			forget := e.offsiteSpec(spec, t, name, append([]string{"forget", "--prune"}, forgetArgs...), nil, false)
			if err := e.Run.RunContainer(ctx, node, "offsite-"+name+"-forget", forget); err != nil {
				return describe("off-site forget pass", err)
			}
			return nil
		})
}

// RestoreOffsite restores planName's latest OFF-SITE snapshot into the
// fresh volume "<plan>-restore" — the recovery path when the local copy
// is gone too. Same restore-alongside rule as RestoreAlongside: nothing
// is written over live data.
func (e *Engine) RestoreOffsite(ctx context.Context, planName string) error {
	ctx = context.WithoutCancel(ctx)
	r, desired, err := e.findPlan(planName)
	if err != nil {
		return err
	}
	if r.BackupPlan.Offsite == nil {
		return fmt.Errorf("backup: plan %q has no off-site copy to restore from", planName)
	}
	return e.restoreOffsite(ctx, r, desired)
}

func (e *Engine) restoreOffsite(ctx context.Context, r resource.Resource, desired []resource.Resource) error {
	name := r.Metadata.Name
	return e.withProvenance(ctx, r, runtimedb.BackupRunOffsiteRestore,
		fmt.Sprintf("off-site restore alongside: %s (BackupTarget %s latest -> volume %s-restore)", name, r.BackupPlan.Offsite.Target, name),
		func(ctx context.Context) error {
			t, err := findTarget(desired, r)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, e.offsiteTimeout())
			defer cancel()
			cs := e.offsiteSpec(r.BackupPlan, t, name, []string{"restore", "latest", "--target", "/restore"}, nil, false)
			cs.Mounts = append(cs.Mounts, resource.Mount{Source: name + "-restore", Target: "/restore"})
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "restore-offsite-"+name, cs); err != nil {
				return describe("off-site restore pass", err)
			}
			return nil
		})
}
