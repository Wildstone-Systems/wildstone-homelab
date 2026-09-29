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

// defaultS3ProbeImage is multi-architecture and digest-pinned like every
// other image Keystone asks Wardstone to run.
const defaultS3ProbeImage = "public.ecr.aws/aws-cli/aws-cli:2.31.21@sha256:40033dc921634b1073094712ea8237869bc857cd7ddc2571896ec9b14ef97ae8"

// TestTarget checks credentials, endpoint reachability and bucket access
// without writing an object. The target may be tested before any plan uses
// it, which is why this uses S3 HeadBucket rather than a restic repository.
func (e *Engine) TestTarget(ctx context.Context, name string) error {
	desired, err := e.State.Load()
	if err != nil {
		return fmt.Errorf("loading desired state: %w", err)
	}
	var target resource.Resource
	for _, r := range desired {
		if r.Kind == resource.KindBackupTarget && r.Metadata.Name == name {
			target = r
			break
		}
	}
	if target.BackupTarget == nil {
		return fmt.Errorf("no BackupTarget %q in the Statefile", name)
	}
	t := target.BackupTarget
	image := e.S3ProbeImage
	if image == "" {
		image = defaultS3ProbeImage
	}
	env := map[string]string{
		"AWS_ACCESS_KEY_ID":     t.AccessKeySecret,
		"AWS_SECRET_ACCESS_KEY": t.SecretKeySecret,
	}
	if t.Region != "" {
		env["AWS_DEFAULT_REGION"] = t.Region
	}
	command := []string{"--no-cli-pager", "--endpoint-url", t.Endpoint, "s3api", "head-bucket", "--bucket", t.Bucket}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := e.Run.RunContainer(ctx, target.Metadata.Node, "test-target-"+name, resource.ContainerSpec{
		Image: image, Env: env, Command: command, Restart: "no",
	}); err != nil {
		return fmt.Errorf("read-only S3 bucket check: %w", err)
	}
	return nil
}

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
		mounts = append(mounts, resource.Mount{Source: v, Target: sourceTarget(v), ReadOnly: true})
	}
	cmd := append([]string{}, command...)
	// An append-only credential cannot remove restic's temporary lock
	// object. Runs are serialized per plan, so disabling repository locks
	// here is safe and avoids accumulating a stale lock that would stop the
	// next backup. Destructive maintenance is deliberately not run with
	// this credential (see runOffsite below).
	if t.AppendOnly {
		cmd = append(cmd, "--no-lock")
	}
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
//
// direct backs the sources up straight into the off-site repository even
// on route local-and-offsite: the fallback when the local copy had no
// room (ADR-W045 A3.1). The same repository takes both, so later copies
// carry on from there.
func (e *Engine) runOffsite(ctx context.Context, r resource.Resource, desired []resource.Resource, direct bool) error {
	spec := r.BackupPlan
	name := r.Metadata.Name
	o := spec.Offsite
	route := o.Route
	if direct {
		route += ", direct: the local copy had no room"
	}
	return e.withProvenance(ctx, r, runtimedb.BackupRunOffsite,
		fmt.Sprintf("off-site run: %s (%s -> BackupTarget %s)", name, route, o.Target),
		func(ctx context.Context) error {
			if name == "wildstone" {
				if e.PrepareRecovery == nil {
					return fmt.Errorf("whole-site recovery builder is not configured")
				}
				if err := e.PrepareRecovery(ctx, r); err != nil {
					return fmt.Errorf("building whole-site recovery set: %w", err)
				}
			}
			t, err := findTarget(desired, r)
			if err != nil {
				return err
			}
			local := o.Route == resource.BackupRouteLocalAndOffsite
			// A copy reads only the local repository; a direct or
			// offsite-only backup reads only the sources.
			copying := local && !direct
			ctx, cancel := context.WithTimeout(ctx, e.offsiteTimeout())
			defer cancel()
			if err := e.checkMounts(ctx, r, desired, !copying, copying); err != nil {
				return err
			}
			if !local {
				e.sampleSources(ctx, r)
			}
			if !copying {
				if err := e.runPreBackup(ctx, r, desired); err != nil {
					return err
				}
			}
			node := r.Metadata.Node

			probe := e.offsiteSpec(spec, t, name, []string{"cat", "config"}, nil, false)
			if err := e.Run.RunContainer(ctx, node, "offsite-"+name+"-probe", probe); err != nil {
				if code, ok := exitCode(err); !ok || code != resticExitNoRepo {
					return describe("off-site repository check", err)
				}
				// Matching chunker parameters is what keeps a copied
				// snapshot deduplicated off-site: without them every
				// copied file would be split differently and re-uploaded.
				initSpec := e.offsiteSpec(spec, t, name, []string{"init"}, nil, false)
				if local {
					initSpec = e.offsiteSpec(spec, t, name, []string{"init", "--copy-chunker-params"}, nil, true)
				}
				err := e.Run.RunContainer(ctx, node, "offsite-"+name+"-init", initSpec)
				if code, ok := exitCode(err); ok && code == resticExitNoRepo && local && direct {
					// A direct run on a plan whose local copy never got
					// made: no local chunker parameters to copy. A plain
					// init still works; later copies just dedupe less.
					err = e.Run.RunContainer(ctx, node, "offsite-"+name+"-init", e.offsiteSpec(spec, t, name, []string{"init"}, nil, false))
				}
				if err != nil {
					return describe("off-site repository init", err)
				}
				e.log().Info("backup: initialized an off-site repository", "plan", name, "target", o.Target)
			}

			send, pass := e.offsiteSpec(spec, t, name, backupCommand(spec), sources(spec), false), "off-site backup pass"
			if local && !direct {
				// Only snapshots made under the current sources and
				// excludes: an older one may hold what is now excluded.
				send, pass = e.offsiteSpec(spec, t, name, []string{"copy", "--tag", configTag(spec)}, nil, true), "off-site copy pass"
			}
			if err := e.Run.RunContainer(ctx, node, "offsite-"+name, send); err != nil {
				return describe(pass, err)
			}

			// forget and prune require delete access. Running them with an
			// append-only node key would either fail against Object Lock or,
			// if the key were accidentally over-privileged, defeat the reason
			// for append-only mode. Maintenance uses a separate offline key.
			if t.AppendOnly {
				e.log().Info("backup: off-site retention is append-only; skipping forget/prune", "plan", name, "target", o.Target)
				return nil
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

// runOffsiteCheck verifies repository structure and a bounded rotating
// subset of pack data. One twentieth caps routine download cost while
// repeated weekly checks eventually read the whole repository.
func (e *Engine) runOffsiteCheck(ctx context.Context, r resource.Resource, desired []resource.Resource) error {
	name := r.Metadata.Name
	subset := e.offsiteCheckSubset()
	return e.withProvenance(ctx, r, runtimedb.BackupRunOffsiteCheck,
		fmt.Sprintf("repository integrity check: %s (off-site, data subset %s)", name, subset),
		func(ctx context.Context) error {
			t, err := findTarget(desired, r)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, e.offsiteTimeout())
			defer cancel()
			cs := e.offsiteSpec(r.BackupPlan, t, name, []string{"check", "--read-data-subset=" + subset}, nil, false)
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "check-offsite-"+name, cs); err != nil {
				return describe("off-site repository integrity check", err)
			}
			return nil
		})
}

// offsiteCheckSubset rotates through twenty stable restic partitions. The
// check reads at most about 5% each week and covers every partition over a
// twenty-week cycle instead of repeatedly sampling the same packs.
func (e *Engine) offsiteCheckSubset() string {
	week := e.now().UTC().Unix() / int64((7 * 24 * time.Hour).Seconds())
	part := week%20 + 1
	if part < 1 {
		part += 20
	}
	return fmt.Sprintf("%d/20", part)
}

// runOffsiteDrill proves the S3 copy can restore into a disposable volume.
// It is scheduled independently from the local drill and from an operator's
// off-site restore, so neither can accidentally reset the other's clock.
func (e *Engine) runOffsiteDrill(ctx context.Context, r resource.Resource, desired []resource.Resource) error {
	name := r.Metadata.Name
	return e.withProvenance(ctx, r, runtimedb.BackupRunOffsiteDrill,
		fmt.Sprintf("off-site restore drill: %s (latest -> volume %s-offsite-drill)", name, name),
		func(ctx context.Context) error {
			t, err := findTarget(desired, r)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(ctx, e.offsiteTimeout())
			defer cancel()
			cs := e.offsiteSpec(r.BackupPlan, t, name, []string{"restore", "latest", "--target", "/restore"}, nil, false)
			cs.Mounts = append(cs.Mounts, resource.Mount{Source: name + "-offsite-drill", Target: "/restore"})
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "drill-offsite-"+name, cs); err != nil {
				return describe("off-site restore drill", err)
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
