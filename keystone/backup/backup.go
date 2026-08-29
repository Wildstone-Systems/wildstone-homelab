// Package backup is the E6 backup engine (ADR-W011 A4). It acts on the
// BackupPlan declarations in the Statefile: on its own timer duty (not
// cron on any host) it runs restic as an owner-labeled one-shot CONTAINER
// with declared mounts — never a host binary — snapshots the plan's
// volumes into the plan's repository volume, applies the retention
// policy, and runs the monthly restore drill. Every run and every drill
// is recorded three ways: a plans-table record driven through the normal
// state machine by the system/backup principal, events on the operational
// stream, and a row in the backup_runs ledger the scheduler reads.
//
// Approval model: committing a BackupPlan through an approved plan IS the
// owner's standing approval for its runs and its restore drills — both
// are restore-alongside-safe (sources mounted read-only; drills restore
// into a scratch volume, never over live data). An IN-PLACE restore is
// deliberately not implemented here: per ADR-W011 A4 it only ever happens
// via an explicit High-risk plan a human approves, which is a later work
// package. RestoreAlongside is the implemented recovery path: latest
// snapshot into a fresh "<plan>-restore" volume, from which the owner (or
// a future guided flow) moves data deliberately.
//
// The engine never parses restic output and the op surface exposes no
// container exit code yet — a run "succeeds" when its container runs to
// completion under the Runner (see Runner). An exit-code arm on the
// protocol is the known follow-up (a minor-7 candidate) that will make
// success reporting truthful to restic's own verdict.
package backup

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"context"
	"encoding/json"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/statefile"
)

// Principal marks every plan record the backup engine drives. The
// reconcile loop skips records with this principal (see loop.Tick): they
// are container runs the engine executes itself, not Statefile intents.
const Principal = "system/backup"

// Runner runs one one-shot container to completion on a node. The
// production implementation is ApplierRunner (EnsureContainer, poll until
// exited, RemoveContainer); tests substitute a fake. A nil error means
// the container ran and finished; the run's effect on the repository is
// taken on that basis (see the package doc on exit codes).
type Runner interface {
	RunContainer(ctx context.Context, node, name string, spec resource.ContainerSpec) error
}

// Engine is the backup service. All fields except Log are required.
type Engine struct {
	State *statefile.Store
	DB    *runtimedb.DB
	Run   Runner
	// ResticImage is the digest-pinned restic image every run uses —
	// engine-level configuration, not per-plan, so one upgrade covers the
	// fleet and a plan cannot smuggle in an arbitrary image.
	ResticImage string
	// Now is the clock seam for tests. nil = time.Now.
	Now func() time.Time
	// DrillEvery is the restore-drill cadence. <= 0 uses 30 days
	// (ADR-W011 A4: "monthly restore-drill").
	DrillEvery time.Duration
	Log        *slog.Logger
}

// scheduleIntervals maps BackupPlanSpec.Schedule to a due interval.
var scheduleIntervals = map[string]time.Duration{
	"daily":   24 * time.Hour,
	"weekly":  7 * 24 * time.Hour,
	"monthly": 30 * 24 * time.Hour,
}

// RunReport describes one executed run for Tick's outcome.
type RunReport struct {
	Plan string
	Kind runtimedb.BackupRunKind
	Err  string // "" = succeeded
}

// Tick evaluates every BackupPlan in the Statefile once: any plan whose
// last successful backup (or drill) is older than its cadence runs now.
// Failures are reported per plan and never stop the others; a failed run
// leaves no successful ledger row, so it is retried on the next tick
// rather than waiting out a full interval.
func (e *Engine) Tick(ctx context.Context) ([]RunReport, error) {
	desired, err := e.State.Load()
	if err != nil {
		return nil, fmt.Errorf("backup: loading statefile: %w", err)
	}
	var reports []RunReport
	for _, r := range desired {
		if r.Kind != resource.KindBackupPlan {
			continue
		}
		if e.due(r.Metadata.Name, runtimedb.BackupRunBackup, scheduleIntervals[r.BackupPlan.Schedule]) {
			err := e.runBackup(ctx, r)
			reports = append(reports, report(r.Metadata.Name, runtimedb.BackupRunBackup, err))
		}
		if e.due(r.Metadata.Name, runtimedb.BackupRunDrill, e.drillEvery()) {
			err := e.runDrill(ctx, r)
			reports = append(reports, report(r.Metadata.Name, runtimedb.BackupRunDrill, err))
		}
	}
	return reports, nil
}

func report(plan string, kind runtimedb.BackupRunKind, err error) RunReport {
	rep := RunReport{Plan: plan, Kind: kind}
	if err != nil {
		rep.Err = err.Error()
	}
	return rep
}

func (e *Engine) drillEvery() time.Duration {
	if e.DrillEvery > 0 {
		return e.DrillEvery
	}
	return 30 * 24 * time.Hour
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// due reports whether (plan, kind) has no successful ledger row within
// interval. An unknown schedule value maps to interval 0 and is never due
// — resource validation makes that unreachable for committed plans.
func (e *Engine) due(plan string, kind runtimedb.BackupRunKind, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	last, err := e.DB.LastBackupRun(plan, kind, runtimedb.BackupRunSucceeded)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return true
	}
	if err != nil {
		e.log().Error("backup: reading run ledger", "plan", plan, "err", err)
		return false // fail safe: don't run on a broken ledger read
	}
	return e.now().Sub(last.StartedAt) >= interval
}

// runBackup executes one backup run: an init pass whose failure is
// tolerated (restic init fails when the repository already exists — that
// is the expected steady state, and there is no side-effect-free "does it
// exist" op to ask first), a backup pass that must succeed, and — when
// the plan declares any retention — a forget --prune pass that must
// succeed too.
func (e *Engine) runBackup(ctx context.Context, r resource.Resource) error {
	spec := r.BackupPlan
	name := r.Metadata.Name
	return e.withProvenance(ctx, r, runtimedb.BackupRunBackup,
		fmt.Sprintf("backup run: %s (%d volume(s) -> %s)", name, len(spec.Volumes), spec.Repository),
		func(ctx context.Context) error {
			initSpec := e.resticSpec(spec, []string{"init"}, nil)
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "backup-"+name+"-init", initSpec); err != nil {
				e.log().Debug("backup: init pass failed (expected once the repository exists)", "plan", name, "err", err)
			}
			backupSpec := e.resticSpec(spec, append([]string{"backup"}, sourceTargets(spec)...), spec.Volumes)
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "backup-"+name, backupSpec); err != nil {
				return fmt.Errorf("backup pass: %w", err)
			}
			forgetArgs := retentionArgs(spec.Retention)
			if forgetArgs == nil {
				return nil
			}
			forgetSpec := e.resticSpec(spec, append([]string{"forget", "--prune"}, forgetArgs...), nil)
			if err := e.Run.RunContainer(ctx, r.Metadata.Node, "backup-"+name+"-forget", forgetSpec); err != nil {
				return fmt.Errorf("forget pass: %w", err)
			}
			return nil
		})
}

// runDrill executes the restore drill: latest snapshot restored alongside
// into the scratch volume "<plan>-drill" — live data is never touched.
func (e *Engine) runDrill(ctx context.Context, r resource.Resource) error {
	name := r.Metadata.Name
	return e.withProvenance(ctx, r, runtimedb.BackupRunDrill,
		fmt.Sprintf("restore drill: %s (latest -> volume %s-drill)", name, name),
		func(ctx context.Context) error {
			return e.restoreInto(ctx, r, name+"-drill", "drill-"+name)
		})
}

// RunNow executes planName's backup immediately, schedule
// notwithstanding, with the same provenance as a scheduled run — the
// on-demand path behind POST /api/v1/backups/{plan}/run (ADR-W015 A2).
// A successful RunNow also resets the schedule clock: the ledger row it
// writes is the "last success" the next due() check reads.
func (e *Engine) RunNow(ctx context.Context, planName string) error {
	r, err := e.findPlan(planName)
	if err != nil {
		return err
	}
	return e.runBackup(ctx, r)
}

// RestoreAlongside restores planName's latest snapshot into the fresh
// volume "<plan>-restore" — the implemented recovery path (ADR-W011 A4:
// restore-alongside first). It runs immediately with full provenance; the
// owner moves data from the restore volume deliberately.
//
// It records runKind "restore", not "drill": mechanically this is the
// drill, but the ledger answers "what happened here", and an operator
// restoring during an incident is not running a rehearsal. Keeping the
// kinds distinct also means a manual restore no longer resets the
// scheduled drill's clock — due() reads the last successful *drill*.
func (e *Engine) RestoreAlongside(ctx context.Context, planName string) error {
	r, err := e.findPlan(planName)
	if err != nil {
		return err
	}
	return e.withProvenance(ctx, r, runtimedb.BackupRunRestore,
		fmt.Sprintf("restore alongside: %s (latest -> volume %s-restore)", planName, planName),
		func(ctx context.Context) error {
			return e.restoreInto(ctx, r, planName+"-restore", "restore-"+planName)
		})
}

func (e *Engine) findPlan(planName string) (resource.Resource, error) {
	desired, err := e.State.Load()
	if err != nil {
		return resource.Resource{}, fmt.Errorf("backup: loading statefile: %w", err)
	}
	for _, r := range desired {
		if r.Kind == resource.KindBackupPlan && r.Metadata.Name == planName {
			return r, nil
		}
	}
	return resource.Resource{}, fmt.Errorf("backup: no BackupPlan %q in the Statefile", planName)
}

func (e *Engine) restoreInto(ctx context.Context, r resource.Resource, targetVolume, containerName string) error {
	spec := *r.BackupPlan
	cs := e.resticSpec(&spec, []string{"restore", "latest", "--target", "/restore"}, nil)
	cs.Mounts = append(cs.Mounts, resource.Mount{Source: targetVolume, Target: "/restore"})
	return e.Run.RunContainer(ctx, r.Metadata.Node, containerName, cs)
}

// withProvenance wraps one run in its full record set: a plans-table
// record (proposed -> approved -> applying by the system/backup principal
// — the standing approval carried by the committed BackupPlan itself),
// the backup_runs ledger row, events, and the terminal transition.
func (e *Engine) withProvenance(ctx context.Context, r resource.Resource, kind runtimedb.BackupRunKind, summary string, fn func(context.Context) error) error {
	stored, err := json.Marshal(map[string]any{"summary": summary, "backupPlan": r.Metadata.Name, "kind": string(kind)})
	if err != nil {
		return err
	}
	planID, err := e.DB.InsertPlan(Principal, "", summary, "low", string(stored))
	if err != nil {
		return fmt.Errorf("backup: recording plan: %w", err)
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
		if err := e.DB.TransitionPlan(planID, st, Principal); err != nil {
			return fmt.Errorf("backup: plan %s -> %s: %w", planID, st, err)
		}
	}
	seq, err := e.DB.StartBackupRun(r.Metadata.Name, kind, planID)
	if err != nil {
		return fmt.Errorf("backup: opening run ledger: %w", err)
	}
	e.event("backup.started", r.Metadata.Node, summary, map[string]string{"plan": planID, "backupPlan": r.Metadata.Name, "kind": string(kind)})

	runErr := fn(ctx)

	status, planState, evt := runtimedb.BackupRunSucceeded, runtimedb.PlanApplied, "backup.completed"
	detail := ""
	if runErr != nil {
		status, planState, evt = runtimedb.BackupRunFailed, runtimedb.PlanFailed, "backup.failed"
		detail = runErr.Error()
	}
	if err := e.DB.FinishBackupRun(seq, status, detail); err != nil {
		e.log().Error("backup: closing run ledger", "seq", seq, "err", err)
	}
	if err := e.DB.TransitionPlan(planID, planState, Principal); err != nil {
		e.log().Error("backup: terminal plan transition", "plan", planID, "err", err)
	}
	e.event(evt, r.Metadata.Node, summary, map[string]string{"plan": planID, "backupPlan": r.Metadata.Name, "kind": string(kind), "detail": detail})
	return runErr
}

// resticSpec composes the one-shot restic container for one pass: every
// source volume read-only under /backup/<vol>, the repository volume
// read-write under /repo, and the repository password as a secret
// REFERENCE only (Wardstone materializes it, E8 — the cloudflared
// TUNNEL_TOKEN posture). withSources nil mounts no sources (init/forget/
// restore passes don't need them).
func (e *Engine) resticSpec(spec *resource.BackupPlanSpec, command []string, withSources []string) resource.ContainerSpec {
	mounts := []resource.Mount{{Source: spec.Repository, Target: "/repo"}}
	for _, v := range withSources {
		mounts = append(mounts, resource.Mount{Source: v, Target: "/backup/" + v, ReadOnly: true})
	}
	return resource.ContainerSpec{
		Image: e.ResticImage,
		Env: map[string]string{
			"RESTIC_REPOSITORY": "/repo",
			"RESTIC_PASSWORD":   spec.PasswordSecret,
		},
		Mounts:  mounts,
		Command: command,
		Restart: "no",
	}
}

// sourceTargets returns the in-container backup paths for the plan's
// volumes, sorted so the restic command line — and with it the container
// spec hash — is deterministic for a given volume set.
func sourceTargets(spec *resource.BackupPlanSpec) []string {
	out := make([]string, 0, len(spec.Volumes))
	for _, v := range spec.Volumes {
		out = append(out, "/backup/"+v)
	}
	sort.Strings(out)
	return out
}

// retentionArgs maps a BackupRetention to restic forget flags; nil when
// every keep count is zero (no forget pass runs at all).
func retentionArgs(ret resource.BackupRetention) []string {
	var args []string
	if ret.KeepDaily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(ret.KeepDaily))
	}
	if ret.KeepWeekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(ret.KeepWeekly))
	}
	if ret.KeepMonthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(ret.KeepMonthly))
	}
	return args
}

func (e *Engine) event(typ, node, msg string, attrs map[string]string) {
	if _, err := e.DB.AppendEvent(runtimedb.Event{Type: typ, Node: node, Message: msg, Attrs: attrs}); err != nil {
		e.log().Error("backup: event append failed", "type", typ, "err", err)
	}
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// RunLoop ticks the engine every interval until ctx is done — the timer
// duty ADR-W011 A4 places in Keystone instead of cron on a host.
func (e *Engine) RunLoop(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		reports, err := e.Tick(ctx)
		if err != nil {
			e.log().Error("backup tick", "err", err)
			continue
		}
		for _, rep := range reports {
			if rep.Err != "" {
				e.log().Warn("backup run failed", "plan", rep.Plan, "kind", string(rep.Kind), "err", rep.Err)
			} else {
				e.log().Info("backup run completed", "plan", rep.Plan, "kind", string(rep.Kind))
			}
		}
	}
}
